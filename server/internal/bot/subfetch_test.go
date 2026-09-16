package bot

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// hostClient sends every request to srv, ignoring the request URL host so a
// WAN miss can fall through to the tunnel server on the same rawURL.
func hostClient(srv *httptest.Server) *http.Client {
	base := srv.Client()
	return &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			clone := req.Clone(req.Context())
			u, err := url.Parse(srv.URL)
			if err != nil {
				return nil, err
			}
			clone.URL.Scheme = u.Scheme
			clone.URL.Host = u.Host
			return base.Transport.RoundTrip(clone)
		}),
	}
}

func TestFetchSubscription_WANSuccess(t *testing.T) {
	var wanHits, tunHits int
	wan := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wanHits++
		_, _ = io.WriteString(w, "wan-body")
	}))
	t.Cleanup(wan.Close)
	tun := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tunHits++
		_, _ = io.WriteString(w, "tun-body")
	}))
	t.Cleanup(tun.Close)

	body, err := fetchSubscription(context.Background(), wan.URL, hostClient(wan), hostClient(tun))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "wan-body" {
		t.Fatalf("body %q, want wan-body", body)
	}
	if wanHits != 1 {
		t.Fatalf("wan hits %d, want 1", wanHits)
	}
	if tunHits != 0 {
		t.Fatalf("tunnel must not run after a WAN success; hits %d", tunHits)
	}
}

func TestFetchSubscription_WANErrorNilTunnel(t *testing.T) {
	wan := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "wan-down", http.StatusBadGateway)
	}))
	t.Cleanup(wan.Close)

	body, err := fetchSubscription(context.Background(), wan.URL, hostClient(wan), nil)
	if err == nil {
		t.Fatal("expected WAN error")
	}
	if body != nil {
		t.Fatalf("non-200 must not return a body, got %q", body)
	}
}

func TestFetchSubscription_WANErrorTunnelSuccess(t *testing.T) {
	var wanHits, tunHits int
	wan := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wanHits++
		http.Error(w, "wan-down", http.StatusBadGateway)
	}))
	t.Cleanup(wan.Close)
	tun := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tunHits++
		_, _ = io.WriteString(w, "tun-body")
	}))
	t.Cleanup(tun.Close)

	body, err := fetchSubscription(context.Background(), "http://subscription.example/list", hostClient(wan), hostClient(tun))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "tun-body" {
		t.Fatalf("body %q, want tun-body", body)
	}
	if wanHits != 1 {
		t.Fatalf("WAN hits %d, want 1", wanHits)
	}
	if tunHits != 1 {
		t.Fatalf("tunnel hits %d, want 1", tunHits)
	}
}

func TestFetchWANThenOptionalTunnel_DoesNotRetryWAN(t *testing.T) {
	var wanHits, tunHits, factoryCalls int
	wanErr := errors.New("wan down")
	wan := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		wanHits++
		return nil, wanErr
	})}
	tun := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tunHits++
		_, _ = io.WriteString(w, "tun-body")
	}))
	t.Cleanup(tun.Close)

	body, err := fetchWANThenOptionalTunnel(context.Background(), "http://subscription.example/list", wan, func() *http.Client {
		factoryCalls++
		return hostClient(tun)
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "tun-body" {
		t.Fatalf("body %q, want tun-body", body)
	}
	if wanHits != 1 {
		t.Fatalf("WAN hits %d; after WAN fails must not retry WAN", wanHits)
	}
	if factoryCalls != 1 {
		t.Fatalf("tunnel factory %d, want 1", factoryCalls)
	}
	if tunHits != 1 {
		t.Fatalf("tunnel hits %d, want 1", tunHits)
	}
}

func TestFetchWANThenOptionalTunnel_SuccessSkipsTunnelFactory(t *testing.T) {
	var factoryCalls int
	wan := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "wan-body")
	}))
	t.Cleanup(wan.Close)

	body, err := fetchWANThenOptionalTunnel(context.Background(), wan.URL, hostClient(wan), func() *http.Client {
		factoryCalls++
		t.Error("tunnel factory must not run after a WAN success")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "wan-body" {
		t.Fatalf("body %q", body)
	}
	if factoryCalls != 0 {
		t.Fatalf("factory calls %d", factoryCalls)
	}
}

func TestFetchSubscription_Non200DoesNotReturnBody(t *testing.T) {
	payload := "not-a-subscription"
	wan := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, payload)
	}))
	t.Cleanup(wan.Close)
	tun := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, payload)
	}))
	t.Cleanup(tun.Close)

	body, err := fetchSubscription(context.Background(), "http://subscription.example/list", hostClient(wan), hostClient(tun))
	if err == nil {
		t.Fatal("expected error on HTTP non-200")
	}
	if body != nil {
		t.Fatalf("non-200 must not return a body to decode, got %q", body)
	}
	if !strings.Contains(err.Error(), "500") && !strings.Contains(err.Error(), "403") {
		t.Fatalf("error %v should mention the HTTP status", err)
	}
}

func TestFetchSubscription_CapsBody(t *testing.T) {
	wan := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("A"), (1<<20)+64))
	}))
	t.Cleanup(wan.Close)

	body, err := fetchSubscription(context.Background(), wan.URL, hostClient(wan), nil)
	if err == nil || !strings.Contains(err.Error(), "subscription body exceeds 1 MiB") {
		t.Fatalf("err %v, want the 1 MiB cap", err)
	}
	if body != nil {
		t.Fatalf("an oversized list must not come back truncated, got %d bytes", len(body))
	}
}

func TestFetchSubscription_WANDialErrorNilTunnel(t *testing.T) {
	wanErr := errors.New("wan down")
	wan := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, wanErr
	})}
	body, err := fetchSubscription(context.Background(), "http://subscription.example/list", wan, nil)
	if !errors.Is(err, wanErr) {
		t.Fatalf("err %v, want WAN error", err)
	}
	if body != nil {
		t.Fatalf("body %q", body)
	}
}
