package watchdapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// AutomationSource serves watch state and durable notification delivery progress.
type AutomationSource interface {
	WatchSnapshot() WatchSnapshot
	ReplaceRecipients([]Recipient) error
	Pending(string) (NotificationPage, error)
	Ack(int64, EventID) error
}

// Validation errors distinguish bad IPC input from storage failures in a source.
var (
	ErrInvalidCursor  = errors.New("invalid notification cursor")
	ErrInvalidEventID = errors.New("invalid notification event ID")
)

const (
	notificationRequestLimit   = 1 << 20
	notificationResponseLimit  = 16 << 20
	notificationPageLimit      = 100
	notificationCursorLimit    = 256
	notificationRequestTimeout = 2 * time.Second
)

type recipientsRequest struct {
	Recipients []Recipient `json:"recipients"`
}

type ackRequest struct {
	ChatID  int64   `json:"chat_id"`
	EventID EventID `json:"event_id"`
}

type mutationResponse struct {
	OK bool `json:"ok"`
}

func addNotificationHandlers(mux *http.ServeMux, src AutomationSource) {
	mux.HandleFunc("GET /v1/watch", func(w http.ResponseWriter, _ *http.Request) {
		writeNotificationJSON(w, src.WatchSnapshot())
	})
	mux.HandleFunc("POST /v1/notifications/recipients", func(w http.ResponseWriter, r *http.Request) {
		var req *recipientsRequest
		if !readNotificationBody(w, r, &req) {
			return
		}
		if req == nil || !validNotificationRecipients(req.Recipients) {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid recipients"})
			return
		}
		if err := src.ReplaceRecipients(req.Recipients); err != nil {
			writeNotificationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, mutationResponse{OK: true})
	})
	mux.HandleFunc("GET /v1/notifications/pending", func(w http.ResponseWriter, r *http.Request) {
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil || len(query["cursor"]) > 1 || len(query.Get("cursor")) > notificationCursorLimit {
			writeNotificationError(w, ErrInvalidCursor)
			return
		}
		page, err := src.Pending(query.Get("cursor"))
		if err != nil {
			writeNotificationError(w, err)
			return
		}
		if len(page.Messages) > notificationPageLimit || len(page.NextCursor) > notificationCursorLimit {
			writeNotificationError(w, errors.New("invalid notification page"))
			return
		}
		if page.Messages == nil {
			page.Messages = []Notification{}
		}
		writeNotificationJSON(w, page)
	})
	mux.HandleFunc("POST /v1/notifications/ack", func(w http.ResponseWriter, r *http.Request) {
		var req *ackRequest
		if !readNotificationBody(w, r, &req) {
			return
		}
		if req == nil || req.ChatID == 0 || !validNotificationEventID(req.EventID) {
			writeNotificationError(w, ErrInvalidEventID)
			return
		}
		if err := src.Ack(req.ChatID, req.EventID); err != nil {
			writeNotificationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, mutationResponse{OK: true})
	})
}

func readNotificationBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(notificationRequestTimeout))
	defer controller.SetReadDeadline(time.Time{})
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, notificationRequestLimit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, errorResponse{Error: "request body is too large"})
		} else {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid request body"})
		}
		return false
	}
	if err := json.Unmarshal(data, dst); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid request body"})
		return false
	}
	return true
}

func validNotificationRecipients(recipients []Recipient) bool {
	if recipients == nil {
		return false
	}
	seen := make(map[int64]bool, len(recipients))
	now := time.Now()
	for _, recipient := range recipients {
		if recipient.ChatID == 0 || seen[recipient.ChatID] || recipient.FirstSeen.IsZero() || recipient.FirstSeen.After(now) {
			return false
		}
		seen[recipient.ChatID] = true
	}
	return true
}

func validNotificationEventID(id EventID) bool {
	value := string(id)
	if len(value) < 34 || len(value) > 53 || value[32] != ':' {
		return false
	}
	for i := 0; i < 32; i++ {
		if (value[i] < '0' || value[i] > '9') && (value[i] < 'a' || value[i] > 'f') {
			return false
		}
	}
	sequence, err := strconv.ParseUint(value[33:], 10, 64)
	return err == nil && sequence != 0 && strconv.FormatUint(sequence, 10) == value[33:]
}

func writeNotificationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalidCursor):
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: ErrInvalidCursor.Error()})
	case errors.Is(err, ErrInvalidEventID):
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: ErrInvalidEventID.Error()})
	default:
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "notification storage unavailable"})
	}
}

func writeNotificationJSON(w http.ResponseWriter, value any) {
	data, err := json.Marshal(value)
	if err != nil || len(data)+1 >= notificationResponseLimit {
		writeNotificationError(w, errors.New("cannot encode notification response"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
	_, _ = w.Write([]byte{'\n'})
}
