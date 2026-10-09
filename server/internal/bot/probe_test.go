package bot

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/netpath"
)

func TestProbePath_AnyHTTPIsLive(t *testing.T) {
	codes := []int{200, 401, 429, 500}
	for _, code := range codes {
		t.Run(http.StatusText(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/botTOKEN/getMe" {
					t.Errorf("path %s", r.URL.Path)
				}
				w.WriteHeader(code)
			}))
			t.Cleanup(srv.Close)
			err := probePath(context.Background(), srv.URL, "TOKEN", Path{Kind: netpath.KindDirect})
			if err != nil {
				t.Fatalf("code %d: %v", code, err)
			}
		})
	}
}

// Transport strips the request deadline before dialing; the seam must receive the 8s probe budget.
func TestProbePath_DialSharesProbeBudget(t *testing.T) {
	var mu sync.Mutex
	var budget time.Duration
	var hadDeadline bool
	var gotPath Path
	var gotNetwork, gotAddr string
	p := Path{Kind: netpath.KindTunnel, ID: "ovpnc2", Iface: "tun12", Mark: 0x10000}
	dialErr := errors.New("dial skipped")
	dial := func(ctx context.Context, path Path, network, addr string) (net.Conn, error) {
		mu.Lock()
		defer mu.Unlock()
		gotPath, gotNetwork, gotAddr = path, network, addr
		if deadline, ok := ctx.Deadline(); ok {
			hadDeadline = true
			budget = time.Until(deadline)
		}
		return nil, dialErr
	}
	if err := probePathWith(context.Background(), "http://probe.example", "TOKEN", p, dial); !errors.Is(err, dialErr) {
		t.Fatalf("err=%v, want the injected dial error", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !hadDeadline {
		t.Fatal("probe dial had no deadline")
	}
	if budget <= 7*time.Second || budget > 8*time.Second {
		t.Fatalf("dial budget %s, want the 8s probe deadline", budget)
	}
	if !gotPath.ParamsEqual(p) || gotNetwork != "tcp" || gotAddr != "probe.example:80" {
		t.Fatalf("dial path=%+v network=%q addr=%q", gotPath, gotNetwork, gotAddr)
	}
}

func TestProbePathWith_RespectsCallerDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var mu sync.Mutex
	var budget time.Duration
	dial := func(ctx context.Context, p Path, network, addr string) (net.Conn, error) {
		if deadline, ok := ctx.Deadline(); ok {
			mu.Lock()
			budget = time.Until(deadline)
			mu.Unlock()
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	err := probePathWith(ctx, "http://probe.example", "TOKEN", Path{Kind: netpath.KindDirect}, dial)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want context.DeadlineExceeded", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if budget <= 0 || budget > 200*time.Millisecond {
		t.Fatalf("dial budget %s, want the caller's shorter deadline", budget)
	}
}

func TestProbePathWith_UsesInjectedDialForGetMe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/botTOKEN/getMe" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	p := Path{Kind: netpath.KindSOCKS, SOCKSPort: 23456}
	dial := func(ctx context.Context, got Path, network, addr string) (net.Conn, error) {
		if !got.ParamsEqual(p) {
			t.Errorf("path %+v, want %+v", got, p)
		}
		var d net.Dialer
		return d.DialContext(ctx, network, srv.Listener.Addr().String())
	}
	if err := probePathWith(context.Background(), "http://probe.example", "TOKEN", p, dial); err != nil {
		t.Fatalf("any HTTP through the injected path is live: %v", err)
	}
}

func TestProbePath_DialErrorIsDead(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := probePath(ctx, "http://127.0.0.1:1", "TOKEN", Path{Kind: netpath.KindDirect})
	if err == nil {
		t.Fatal("expected error")
	}
}
