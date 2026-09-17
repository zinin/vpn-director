package vpnconfig

import (
	"errors"
	"fmt"
)

// ErrSaveServers marks a PublishServers failure that came from servers.json
// itself, which leaves the bypass list alone. Any other error means the list
// is published and only the config it is read against is not.
var ErrSaveServers = errors.New("save servers")

// PublishServers publishes one imported subscription: servers.json, which the
// UI and the watch walk read, and xray.servers, the bypass set the proxy's own
// endpoints come from. Both are written under the caller's config lock, so two
// importers cannot leave a servers.json from one download beside a bypass list
// from another - save takes no lock of its own.
//
// update is the caller's locked config update and save writes servers.json.
// Every importer goes through here: the Web UI, the bot's /import and the
// subscription watch.
//
// A non-empty subscriptionURL is stored with the list; an empty one leaves a
// saved link in place, so a re-import from it does not clear it.
//
// A non-nil guard runs first, inside the same locked update, on the config the
// lock protects: a publisher whose download is no longer the one the config
// asks for refuses there rather than writing over a newer one.
func PublishServers(update func(func(*VPNDirectorConfig) error) error, save func([]Server) error, servers []Server, subscriptionURL string, guard func(*VPNDirectorConfig) error) error {
	publish := func(cfg *VPNDirectorConfig) error {
		if guard != nil {
			if err := guard(cfg); err != nil {
				return err
			}
		}
		if save != nil {
			if err := save(servers); err != nil {
				return fmt.Errorf("%w: %w", ErrSaveServers, err)
			}
		}
		if cfg == nil {
			return nil
		}
		cfg.Xray.Servers = ServerIPs(servers)
		if subscriptionURL != "" {
			cfg.Xray.SubscriptionURL = subscriptionURL
		}
		return nil
	}
	if update == nil {
		return publish(nil)
	}
	return update(publish)
}
