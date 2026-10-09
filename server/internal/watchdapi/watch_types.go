package watchdapi

import "time"

// WatchState is the subscription automation's state.
type WatchState string

const (
	WatchStarting     WatchState = "starting"
	WatchActive       WatchState = "active"
	WatchStopped      WatchState = "stopped"
	WatchIncompatible WatchState = "incompatible"
	WatchError        WatchState = "error"
	WatchNotRunning   WatchState = "not_running"
)

// EventID is a 32-character lowercase hex store epoch and a positive sequence.
type EventID string

type Recipient struct {
	ChatID    int64     `json:"chat_id"`
	FirstSeen time.Time `json:"first_seen"`
}

type Notification struct {
	ChatID  int64     `json:"chat_id"`
	EventID EventID   `json:"event_id"`
	At      time.Time `json:"at"`
	Text    string    `json:"text"`
}

type NotificationPage struct {
	Messages   []Notification `json:"messages"`
	NextCursor string         `json:"next_cursor"`
}

type NotificationsStatus struct {
	Pending      int    `json:"pending"`
	StorageError string `json:"storage_error"`
}

type WatchSnapshot struct {
	State             WatchState          `json:"state"`
	UpdatedAt         time.Time           `json:"updated_at"`
	Message           string              `json:"message"`
	Action            string              `json:"action"`
	CommittedFailover bool                `json:"committed_failover"`
	PendingRestore    bool                `json:"pending_restore"`
	Notifications     NotificationsStatus `json:"notifications"`
}
