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
// PublishImport publishes a list the Web UI or the bot's /import downloaded from
// fetchURL. An import that named its link stores that link with the list, and
// the pair is consistent by construction. One that re-used the saved link
// (explicitURL empty) publishes only while that link is still fetchURL: another
// importer may have saved a different subscription while this download ran.
func PublishImport(store ConfigStore, servers []vpnconfig.Server, explicitURL, fetchURL string) error {
	if explicitURL != "" {
		return PublishServers(store, servers, explicitURL)
	}
	return publishServers(store, servers, "", vpnconfig.SubscriptionUnchanged(fetchURL))
}

func PublishServers(store ConfigStore, servers []vpnconfig.Server, subscriptionURL string) error {
	return publishServers(store, servers, subscriptionURL, nil)
}

func publishServers(store ConfigStore, servers []vpnconfig.Server, subscriptionURL string, guard func(*vpnconfig.VPNDirectorConfig) error) error {
	err := vpnconfig.PublishServers(store.UpdateVPNConfig, store.SaveServers, servers, subscriptionURL, guard)
	// A guarded publication has a config to compare against, and one it cannot
	// read cannot say the list is still wanted.
	if guard != nil || !errors.Is(err, ErrConfigLoad) {
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
