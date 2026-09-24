// internal/service/subscriptions.go
package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/zinin/vpn-director/server/internal/ssrf"
	"github.com/zinin/vpn-director/server/internal/subscription"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// MaxSubscriptionBody is the largest subscription taken, in bytes. A larger
// one is refused, never cut: cut, a base64 list decodes to a shorter one, and
// that would be published as the subscription.
const MaxSubscriptionBody = 1 << 20

// ErrSubscriptionURL refuses a link the daemons do not download: not https, or
// naming a private or reserved host.
var ErrSubscriptionURL = errors.New("invalid subscription URL")

// DownloadError is a subscription that did not arrive: the connection, the
// HTTP status or the size cap. The Web UI answers it with 502.
type DownloadError struct{ Err error }

func (e *DownloadError) Error() string {
	if errors.Is(e.Err, ssrf.ErrBlockedAddress) {
		// The dial guard's error carries the resolved internal address, and
		// this text goes to the browser and the chat.
		return "download failed: URL resolved to a private or reserved address"
	}
	return "download failed: " + e.Err.Error()
}

func (e *DownloadError) Unwrap() error { return e.Err }

// downloadError wraps a failed download. A *url.Error gives up its URL first:
// its text is the whole link, token included.
func downloadError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return &DownloadError{Err: err}
}

// BodyError is a subscription that arrived and holds no server the router can
// use: nothing it decodes, or nothing whose host resolves. 400 in the Web UI.
type BodyError struct{ Err error }

func (e *BodyError) Error() string { return e.Err.Error() }
func (e *BodyError) Unwrap() error { return e.Err }

// ValidateSubscriptionURL refuses what the daemons do not download: anything
// but an https link, and a link to a private or reserved host. The dial guard
// of ssrf.NewClient is the authoritative check and also defeats DNS
// rebinding; this one gives the clear message early.
func ValidateSubscriptionURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return fmt.Errorf("%w: use an https:// link", ErrSubscriptionURL)
	}
	if ssrf.IsPrivateHost(u.Hostname()) {
		return fmt.Errorf("%w: it must not point to a private or loopback address", ErrSubscriptionURL)
	}
	return nil
}

// DownloadSubscription fetches rawURL through client - the SSRF-hardened one -
// then decodes the body and resolves every host over IPv4, bound to ctx. The
// Web UI and the bot's /import download here; the watch has its own path (the
// WAN, then the tunnel).
func DownloadSubscription(ctx context.Context, client *http.Client, rawURL string) (subscription.Import, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return subscription.Import{}, fmt.Errorf("%w: use an https:// link", ErrSubscriptionURL)
	}
	resp, err := client.Do(req)
	if err != nil {
		return subscription.Import{}, downloadError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return subscription.Import{}, &DownloadError{Err: fmt.Errorf("HTTP %d", resp.StatusCode)}
	}
	// One byte past the cap tells a list that is too long from one that fits exactly.
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxSubscriptionBody+1))
	if err != nil {
		return subscription.Import{}, downloadError(err)
	}
	if len(body) > MaxSubscriptionBody {
		return subscription.Import{}, &DownloadError{Err: errors.New("subscription exceeds 1 MiB")}
	}
	imp, err := subscription.DecodeAndResolveLookup(string(body), subscription.LookupIPv4(ctx))
	switch {
	case err != nil:
		return imp, &BodyError{Err: err}
	case imp.Parsed == 0:
		return imp, &BodyError{Err: errors.New(imp.NoServers())}
	case len(imp.Servers) == 0:
		return imp, &BodyError{Err: errors.New("could not resolve IP for any server")}
	}
	return imp, nil
}

// SubscriptionResult is what one add or refresh came to, for the Web UI and
// the bot to report.
type SubscriptionResult struct {
	ID      string
	Name    string
	Existed bool                // an add that refreshed a saved link
	Import  subscription.Import // what the download decoded to
	Err     error
}

// Line is the one line a result reads as: "Alpha: Imported 32 of 40 servers:
// 7 composite" or "Alpha: download failed: HTTP 403".
func (r SubscriptionResult) Line() string {
	name := r.Name
	if name == "" {
		name = "subscription"
	}
	if r.Err != nil {
		return name + ": " + r.Err.Error()
	}
	return name + ": " + r.Import.Summary()
}

// SubscriptionFilesOf is store's subscription files, for vpnconfig's operations.
func SubscriptionFilesOf(store ConfigStore) vpnconfig.SubscriptionFiles {
	return vpnconfig.SubscriptionFiles{Load: store.LoadSubscriptions, Save: store.SaveSubscription, Delete: store.DeleteSubscription}
}

// AddSubscription downloads rawURL and saves it as a subscription named name,
// its host when name is empty. A link already saved is refreshed instead
// (vpnconfig.AddSubscription).
func AddSubscription(ctx context.Context, store ConfigStore, client *http.Client, rawURL, name string) SubscriptionResult {
	res := SubscriptionResult{}
	if err := ValidateSubscriptionURL(rawURL); err != nil {
		res.Err = err
		return res
	}
	if name != "" {
		clean, err := vpnconfig.CleanSubscriptionName(name)
		if err != nil {
			res.Err = err
			return res
		}
		res.Name = clean
	}
	imp, err := DownloadSubscription(ctx, client, rawURL)
	res.Import = imp
	if err != nil {
		res.Err = err
		return res
	}
	sub, existed, err := vpnconfig.AddSubscription(store.UpdateVPNConfig, SubscriptionFilesOf(store), rawURL, res.Name, imp.Servers, time.Now())
	res.Existed, res.Err = existed, err
	if sub.ID != "" {
		res.ID, res.Name = sub.ID, sub.Name
	}
	return res
}

// RefreshSubscription downloads the saved link of subscription id again and
// publishes what arrived. A download that fails records why in the
// subscription's error, and the list stays.
func RefreshSubscription(ctx context.Context, store ConfigStore, client *http.Client, id string) SubscriptionResult {
	subs, err := store.LoadSubscriptions()
	if err != nil {
		return SubscriptionResult{ID: id, Err: err}
	}
	i := vpnconfig.FindSubscription(subs, id)
	if i < 0 {
		return SubscriptionResult{ID: id, Err: vpnconfig.ErrSubscriptionGone}
	}
	return refresh(ctx, store, client, subs[i])
}

func refresh(ctx context.Context, store ConfigStore, client *http.Client, sub vpnconfig.Subscription) SubscriptionResult {
	res := SubscriptionResult{ID: sub.ID, Name: sub.Name, Existed: true}
	if sub.Static() {
		res.Err = vpnconfig.ErrSubscriptionStatic
		return res
	}
	imp, err := DownloadSubscription(ctx, client, sub.URL)
	res.Import = imp
	if err != nil {
		res.Err = err
		rerr := vpnconfig.RecordSubscriptionError(store.UpdateVPNConfig, SubscriptionFilesOf(store), sub.ID, sub.URL, sub.Refreshed, err.Error())
		if rerr != nil && !errors.Is(rerr, vpnconfig.ErrSubscriptionGone) {
			slog.Warn("Failed to record why a subscription did not refresh", "subscription", sub.Name, "error", rerr)
		}
		return res
	}
	_, res.Err = vpnconfig.RefreshSubscription(store.UpdateVPNConfig, SubscriptionFilesOf(store), sub.ID, sub.URL, imp.Servers, time.Now())
	return res
}

// RefreshAllSubscriptions refreshes every subscription with a link at once and
// answers one result each, in subscription order. Static lists are left out.
func RefreshAllSubscriptions(ctx context.Context, store ConfigStore, client *http.Client) ([]SubscriptionResult, error) {
	subs, err := store.LoadSubscriptions()
	if err != nil {
		return nil, err
	}
	var linked []vpnconfig.Subscription
	for _, s := range subs {
		if !s.Static() {
			linked = append(linked, s)
		}
	}
	results := make([]SubscriptionResult, len(linked))
	var wg sync.WaitGroup
	for i, s := range linked {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = refresh(ctx, store, client, s)
		}()
	}
	wg.Wait()
	return results, nil
}

// RenameSubscription gives subscription id the name name.
func RenameSubscription(store ConfigStore, id, name string) error {
	_, err := vpnconfig.RenameSubscription(store.UpdateVPNConfig, SubscriptionFilesOf(store), id, name)
	return err
}

// DeleteSubscription removes subscription id; activeWasIn says the running
// server came from it (vpnconfig.DeleteSubscription).
func DeleteSubscription(store ConfigStore, id string) (activeWasIn bool, err error) {
	return vpnconfig.DeleteSubscription(store.UpdateVPNConfig, SubscriptionFilesOf(store), id)
}
