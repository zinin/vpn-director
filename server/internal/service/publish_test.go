// internal/service/publish_test.go
package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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
	if !errors.Is(err, vpnconfig.ErrServersSaved) {
		t.Fatalf("err %v; the list is saved, and the error has to say so", err)
	}
	if len(store.savedServers) != 1 {
		t.Fatalf("savedServers %v; an imported list must survive a missing config", store.savedServers)
	}
}

// Only a missing vpn-director.json is the router before its first configure. One
// that is there but does not parse was taken for it too: the list went to the
// default data directory, whatever data_dir the file names, while the bypass
// list and the saved link stayed as they were.
func TestPublishServers_AConfigThatDoesNotParsePublishesNothing(t *testing.T) {
	dir := t.TempDir()
	defaultData := filepath.Join(dir, "data")
	writeTestConfig(t, dir, `{"data_dir": "`+filepath.Join(dir, "usb")+`",}`)
	svc := NewConfigService(dir, defaultData)

	err := PublishServers(svc, []vpnconfig.Server{{IPs: []string{"203.0.113.10"}}}, "https://cdn.example/s/token")

	if !errors.Is(err, ErrConfigLoad) || errors.Is(err, vpnconfig.ErrServersSaved) {
		t.Fatalf("err %v, want a load failure that saved nothing", err)
	}
	if _, serr := os.Stat(filepath.Join(defaultData, "servers.json")); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("servers.json in the default data directory: %v", serr)
	}
}

// A re-import of the saved link needs the config to compare the link against.
// One it cannot read publishes nothing, and says so.
func TestPublishImport_AConfigItCannotReadPublishesNothing(t *testing.T) {
	store := &stubStore{err: fmt.Errorf("%w: %w", ErrConfigLoad, errors.New("invalid character"))}

	err := PublishImport(store, []vpnconfig.Server{{IPs: []string{"1.1.1.1"}}}, "", "https://cdn.example/s/a")

	if !errors.Is(err, ErrConfigLoad) || errors.Is(err, vpnconfig.ErrServersSaved) {
		t.Fatalf("err %v, want a load failure that saved nothing", err)
	}
	if store.savedServers != nil {
		t.Fatalf("savedServers %v", store.savedServers)
	}
}

// A re-import from the saved link publishes only while that link is still the
// saved one: another importer may have saved a different subscription while
// this download ran, and its list must not be replaced by one the saved link no
// longer produces.
func TestPublishImport_RefusesAListFromALinkNoLongerSaved(t *testing.T) {
	store := &stubStore{cfg: &vpnconfig.VPNDirectorConfig{Xray: vpnconfig.XrayConfig{
		SubscriptionURL: "https://cdn.example/s/b",
		Servers:         []string{"198.51.100.1"},
	}}}

	err := PublishImport(store, []vpnconfig.Server{{IPs: []string{"203.0.113.10"}}}, "", "https://cdn.example/s/a")
	if !errors.Is(err, vpnconfig.ErrSubscriptionChanged) {
		t.Fatalf("err %v, want ErrSubscriptionChanged", err)
	}
	if store.savedServers != nil {
		t.Fatalf("savedServers %v; servers.json must keep the newer subscription's list", store.savedServers)
	}
	if !reflect.DeepEqual(store.cfg.Xray.Servers, []string{"198.51.100.1"}) {
		t.Fatalf("Xray.Servers %v", store.cfg.Xray.Servers)
	}
}

// An import that named its link writes that link together with its list, so
// there is nothing to compare: the pair is consistent by construction.
func TestPublishImport_StoresTheLinkItWasGiven(t *testing.T) {
	store := &stubStore{cfg: &vpnconfig.VPNDirectorConfig{Xray: vpnconfig.XrayConfig{SubscriptionURL: "https://cdn.example/s/b"}}}

	err := PublishImport(store, []vpnconfig.Server{{IPs: []string{"203.0.113.10"}}}, "https://cdn.example/s/a", "https://cdn.example/s/a")
	if err != nil {
		t.Fatalf("PublishImport: %v", err)
	}
	if store.cfg.Xray.SubscriptionURL != "https://cdn.example/s/a" || len(store.savedServers) != 1 {
		t.Fatalf("url %q, saved %v", store.cfg.Xray.SubscriptionURL, store.savedServers)
	}
}
