// internal/service/publish_test.go
package service

import (
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

func TestPublishServers_WritesTheListUnderTheStoreLock(t *testing.T) {
	store := &stubStore{cfg: &vpnconfig.VPNDirectorConfig{}}

	if err := PublishServers(store, []vpnconfig.Server{{IPs: []string{"1.1.1.1"}}}, "https://cdn.example/s/token"); err != nil {
		t.Fatalf("PublishServers: %v", err)
	}
	if !store.serversUnderLock {
		t.Fatal("servers.json must be written inside UpdateVPNConfig, which is the cross-process lock")
	}
	if len(store.savedServers) != 1 {
		t.Fatalf("savedServers %v", store.savedServers)
	}
	if store.cfg.Xray.SubscriptionURL != "https://cdn.example/s/token" {
		t.Fatalf("SubscriptionURL %q", store.cfg.Xray.SubscriptionURL)
	}
}

// The bot's /import runs before the first configure, when there is no
// vpn-director.json to sync with. The downloaded list is still what the user
// asked for, so it is kept and only the sync is reported as missing.
func TestPublishServers_KeepsTheListWithoutAConfig(t *testing.T) {
	store := &stubStore{err: fmt.Errorf("%w: %w", ErrConfigLoad, os.ErrNotExist)}

	err := PublishServers(store, []vpnconfig.Server{{IPs: []string{"1.1.1.1"}}}, "")
	if !errors.Is(err, ErrConfigLoad) {
		t.Fatalf("err %v, want ErrConfigLoad", err)
	}
	if len(store.savedServers) != 1 {
		t.Fatalf("savedServers %v; an imported list must survive a missing config", store.savedServers)
	}
}
