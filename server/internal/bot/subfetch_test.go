package bot

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zinin/vpn-director/server/internal/service"
)

func TestSubscriptionFetcher_RejectsNonHTTPS(t *testing.T) {
	servers, err := (service.SubscriptionFetcher{}).Fetch(context.Background(), "http://cdn.example/s/token")
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("err %v, want an https error", err)
	}
	if !errors.Is(err, service.ErrSubscriptionURL) || err.Error() != "invalid subscription URL: use an https:// link" {
		t.Fatalf("err %q, want the daemons' invalid subscription URL", err)
	}
	if strings.Contains(err.Error(), "cdn.example") {
		t.Fatalf("error %q carries the subscription URL", err)
	}
	if servers != nil {
		t.Fatalf("servers %v", servers)
	}
}
