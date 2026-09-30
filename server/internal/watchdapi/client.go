package watchdapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// API is what the Web UI and the bot ask of vpn-director-watchd. *Client
// implements it; the handlers' tests fake it.
type API interface {
	Monitor(ctx context.Context) (Snapshot, error)
	// Check queues the endpoints of keys, every endpoint when keys is empty,
	// and answers how many; ErrNotActive while the monitor is stopped or
	// disabled.
	Check(ctx context.Context, keys []string) (int, error)
}

// clientTimeout bounds every request, so a hung daemon cannot stall a page or
// a command. A var so a test can shorten it.
var clientTimeout = 2 * time.Second

// Client asks the daemon over its unix socket.
type Client struct {
	http *http.Client
}

var _ API = (*Client)(nil)

// NewClient returns a client of the socket at path.
func NewClient(path string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
		DisableKeepAlives: true,
	}
	return &Client{http: &http.Client{Transport: tr}}
}

// Monitor returns the monitor's state.
func (c *Client) Monitor(ctx context.Context) (Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, clientTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://watchd/v1/monitor", nil)
	if err != nil {
		return Snapshot{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Snapshot{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Snapshot{}, fmt.Errorf("vpn-director-watchd answered %d", resp.StatusCode)
	}
	var s Snapshot
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&s); err != nil {
		return Snapshot{}, err
	}
	return s, nil
}

// Check queues the endpoints of keys, every endpoint when keys is empty.
func (c *Client) Check(ctx context.Context, keys []string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, clientTimeout)
	defer cancel()
	body, err := json.Marshal(checkRequest{Keys: keys})
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://watchd/v1/monitor/check", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusAccepted:
		var r checkResponse
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
			return 0, err
		}
		return r.Queued, nil
	case http.StatusConflict:
		return 0, ErrNotActive
	default:
		return 0, fmt.Errorf("vpn-director-watchd answered %d", resp.StatusCode)
	}
}
