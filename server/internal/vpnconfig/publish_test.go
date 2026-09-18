package vpnconfig

import (
	"errors"
	"reflect"
	"testing"
)

// servers.json and xray.servers are read against each other, so they must come
// from the same download even while another importer is publishing its own.
func TestPublishServers_SavesTheListInsideTheConfigUpdate(t *testing.T) {
	cfg := &VPNDirectorConfig{}
	inUpdate := false
	savedUnderLock := false
	update := func(fn func(*VPNDirectorConfig) error) error {
		inUpdate = true
		defer func() { inUpdate = false }()
		return fn(cfg)
	}
	save := func([]Server) error {
		savedUnderLock = inUpdate
		return nil
	}

	if err := PublishServers(update, save, []Server{{IPs: []string{"1.1.1.1"}}}, "", nil); err != nil {
		t.Fatalf("PublishServers: %v", err)
	}
	if !savedUnderLock {
		t.Fatal("servers.json must be written inside the config update, where the lock is held")
	}
	if !reflect.DeepEqual(cfg.Xray.Servers, []string{"1.1.1.1"}) {
		t.Fatalf("Xray.Servers %v", cfg.Xray.Servers)
	}
}

// A lock the importer never got publishes nothing at all: half a publication is
// what the lock exists to prevent.
func TestPublishServers_LockFailureDoesNotWriteTheList(t *testing.T) {
	saved := false
	errLock := errors.New("config lock timeout")
	update := func(func(*VPNDirectorConfig) error) error { return errLock }
	save := func([]Server) error {
		saved = true
		return nil
	}

	err := PublishServers(update, save, []Server{{IPs: []string{"1.1.1.1"}}}, "", nil)
	if !errors.Is(err, errLock) {
		t.Fatalf("err %v, want the lock error", err)
	}
	if saved {
		t.Fatal("servers.json must not be published when the config lock was not taken")
	}
	if errors.Is(err, ErrServersSaved) {
		t.Fatalf("err %v says the list was saved", err)
	}
}

// The importers tell the user the list was imported only when servers.json was
// written. A config write that fails after it leaves the list published and the
// bypass set beside it stale, and the error has to say which of the two it is.
func TestPublishServers_AConfigWriteThatFailsAfterTheListSaysTheListIsSaved(t *testing.T) {
	errWrite := errors.New("save config: no space left on device")
	update := func(fn func(*VPNDirectorConfig) error) error {
		if err := fn(&VPNDirectorConfig{}); err != nil {
			return err
		}
		return errWrite
	}
	saved := false
	save := func([]Server) error {
		saved = true
		return nil
	}

	err := PublishServers(update, save, []Server{{IPs: []string{"1.1.1.1"}}}, "", nil)

	if !saved {
		t.Fatal("the list was not saved")
	}
	if !errors.Is(err, ErrServersSaved) || !errors.Is(err, errWrite) {
		t.Fatalf("err %v, want ErrServersSaved around the write error", err)
	}
}

// Neither a refused guard nor a failed save of servers.json leaves a list behind.
func TestPublishServers_OnlyASavedListSaysSo(t *testing.T) {
	update := func(fn func(*VPNDirectorConfig) error) error { return fn(&VPNDirectorConfig{}) }
	refused := errors.New("refused")

	err := PublishServers(update, func([]Server) error { return nil }, nil, "", func(*VPNDirectorConfig) error { return refused })
	if !errors.Is(err, refused) || errors.Is(err, ErrServersSaved) {
		t.Fatalf("guard refusal: err %v", err)
	}
	err = PublishServers(update, func([]Server) error { return errors.New("disk full") }, nil, "", nil)
	if !errors.Is(err, ErrSaveServers) || errors.Is(err, ErrServersSaved) {
		t.Fatalf("save failure: err %v", err)
	}
}

func TestPublishServers_SaveFailureLeavesTheBypassList(t *testing.T) {
	cfg := &VPNDirectorConfig{Xray: XrayConfig{Servers: []string{"203.0.113.9"}}}
	update := func(fn func(*VPNDirectorConfig) error) error { return fn(cfg) }
	save := func([]Server) error { return errors.New("no space left on device") }

	err := PublishServers(update, save, []Server{{IPs: []string{"1.1.1.1"}}}, "", nil)
	if !errors.Is(err, ErrSaveServers) {
		t.Fatalf("err %v, want ErrSaveServers", err)
	}
	if !reflect.DeepEqual(cfg.Xray.Servers, []string{"203.0.113.9"}) {
		t.Fatalf("Xray.Servers %v; a list that was not saved must not reach the bypass set", cfg.Xray.Servers)
	}
}

func TestPublishServers_KeepsASavedURLWhenNoneIsGiven(t *testing.T) {
	cfg := &VPNDirectorConfig{Xray: XrayConfig{SubscriptionURL: "https://cdn.example/s/token"}}
	update := func(fn func(*VPNDirectorConfig) error) error { return fn(cfg) }

	if err := PublishServers(update, nil, []Server{{IPs: []string{"1.1.1.1"}}}, "", nil); err != nil {
		t.Fatalf("PublishServers: %v", err)
	}
	if cfg.Xray.SubscriptionURL != "https://cdn.example/s/token" {
		t.Fatalf("SubscriptionURL %q", cfg.Xray.SubscriptionURL)
	}
}

func TestRecordActiveServer_CountsWrites(t *testing.T) {
	s := Server{Name: "Oslo", Address: "oslo.example", Port: 443}
	first := RecordActiveServer(nil, s)
	if first.Seq != 1 {
		t.Fatalf("Seq %d, want 1", first.Seq)
	}
	again := RecordActiveServer(first, s)
	if again.Seq != 2 {
		t.Fatalf("Seq %d; re-selecting the running server is still a write", again.Seq)
	}
	if again.Name != "Oslo" || again.Address != "oslo.example" || again.Port != 443 {
		t.Fatalf("record %+v", again)
	}
}

func TestSubscriptionUnchanged(t *testing.T) {
	guard := SubscriptionUnchanged("https://cdn.example/s/a")
	if err := guard(&VPNDirectorConfig{Xray: XrayConfig{SubscriptionURL: "https://cdn.example/s/a"}}); err != nil {
		t.Fatalf("same link refused: %v", err)
	}
	if err := guard(&VPNDirectorConfig{Xray: XrayConfig{SubscriptionURL: "https://cdn.example/s/b"}}); !errors.Is(err, ErrSubscriptionChanged) {
		t.Fatalf("err %v, want ErrSubscriptionChanged", err)
	}
	if err := guard(nil); err != nil {
		t.Fatalf("no config has no saved link to disagree with: %v", err)
	}
}
