package watchdapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

var _ WatchAPI = (*Client)(nil)
var _ NotificationAPI = (*Client)(nil)

// Watch returns the subscription automation's state.
func (c *Client) Watch(ctx context.Context) (WatchSnapshot, error) {
	var snapshot WatchSnapshot
	if err := c.notificationRequest(ctx, http.MethodGet, "/v1/watch", nil, &snapshot); err != nil {
		return WatchSnapshot{}, err
	}
	return snapshot, nil
}

// SetRecipients replaces the full recipient list; nil revokes all recipients.
func (c *Client) SetRecipients(ctx context.Context, recipients []Recipient) error {
	if recipients == nil {
		recipients = []Recipient{}
	}
	return c.notificationMutation(ctx, "/v1/notifications/recipients", recipientsRequest{Recipients: recipients})
}

// Pending reads one page without consuming delivery entries.
func (c *Client) Pending(ctx context.Context, cursor string) (NotificationPage, error) {
	if len(cursor) > notificationCursorLimit {
		return NotificationPage{}, ErrInvalidCursor
	}
	query := url.Values{"cursor": {cursor}}
	var page NotificationPage
	if err := c.notificationRequest(ctx, http.MethodGet, "/v1/notifications/pending?"+query.Encode(), nil, &page); err != nil {
		return NotificationPage{}, err
	}
	if page.Messages == nil || len(page.Messages) > notificationPageLimit || len(page.NextCursor) > notificationCursorLimit {
		return NotificationPage{}, errors.New("invalid notification page")
	}
	return page, nil
}

// Ack confirms only the selected chat/event pair.
func (c *Client) Ack(ctx context.Context, chatID int64, eventID EventID) error {
	return c.notificationMutation(ctx, "/v1/notifications/ack", ackRequest{ChatID: chatID, EventID: eventID})
}

func (c *Client) notificationMutation(ctx context.Context, path string, body any) error {
	var response mutationResponse
	if err := c.notificationRequest(ctx, http.MethodPost, path, body, &response); err != nil {
		return err
	}
	if !response.OK {
		return errors.New("notification mutation was not confirmed")
	}
	return nil
}

func (c *Client) notificationRequest(ctx context.Context, method, path string, body, dst any) error {
	ctx, cancel := context.WithTimeout(ctx, notificationRequestTimeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return errors.New("cannot encode notification request")
		}
		if len(data) > notificationRequestLimit {
			return errors.New("notification request body is too large")
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://watchd"+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, notificationResponseLimit+1))
	if err != nil {
		return err
	}
	if len(data) > notificationResponseLimit {
		return errors.New("notification response is too large")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("vpn-director-watchd answered %d", resp.StatusCode)
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' || json.Unmarshal(data, dst) != nil {
		return errors.New("invalid notification response")
	}
	return nil
}
