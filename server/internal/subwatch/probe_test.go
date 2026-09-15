package subwatch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProbeThrough_Requires204(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ok.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := probeThrough(ctx, ok.Client(), ok.URL); err != nil {
		t.Fatalf("204: %v", err)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer bad.Close()
	if err := probeThrough(ctx, bad.Client(), bad.URL); err == nil {
		t.Fatal("200 must fail")
	}

	down := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	down.Close()
	if err := probeThrough(ctx, down.Client(), down.URL); err == nil {
		t.Fatal("dial error must fail")
	}
}
