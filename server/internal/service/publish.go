// internal/service/publish.go
package service

import (
	"errors"
	"fmt"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// PublishServers writes servers.json and the xray.servers bypass list for one
// imported subscription, under the store's config lock. UpdateVPNConfig is that
// lock; see vpnconfig.PublishServers for why both writes belong inside it.
func PublishServers(store ConfigStore, servers []vpnconfig.Server, subscriptionURL string) error {
	err := vpnconfig.PublishServers(store.UpdateVPNConfig, store.SaveServers, servers, subscriptionURL, nil)
	if !errors.Is(err, ErrConfigLoad) {
		return err
	}
	// There is no vpn-director.json to keep the list in step with: /import runs
	// before the first configure, and until it exists no other writer can be
	// publishing either. Keep the download and report the sync that did not
	// happen.
	if serr := store.SaveServers(servers); serr != nil {
		return fmt.Errorf("%w: %w", vpnconfig.ErrSaveServers, serr)
	}
	return err
}
