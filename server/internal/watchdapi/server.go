package watchdapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Source is what the socket serves: the monitor, in the daemon.
type Source interface {
	Snapshot() Snapshot
	// Request makes the endpoints of keys - every endpoint when keys is empty -
	// due ahead of the rest and answers how many it queued; ErrNotActive
	// while the monitor is stopped or disabled.
	Request(keys []string) (int, error)
}

type checkRequest struct {
	Keys []string `json:"keys"`
}

type checkResponse struct {
	Queued int `json:"queued"`
}

type errorResponse struct {
	Error string `json:"error"`
	State State  `json:"state,omitempty"`
}

// NewHandler serves GET /v1/monitor and POST /v1/monitor/check for src.
func NewHandler(src Source) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/monitor", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, src.Snapshot())
	})
	mux.HandleFunc("POST /v1/monitor/check", func(w http.ResponseWriter, r *http.Request) {
		var req checkRequest
		body := http.MaxBytesReader(w, r.Body, 1<<20)
		// An empty body is a check of every endpoint, like {}.
		if err := json.NewDecoder(body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid request body"})
			return
		}
		n, err := src.Request(req.Keys)
		if errors.Is(err, ErrNotActive) {
			writeJSON(w, http.StatusConflict, errorResponse{Error: err.Error(), State: src.Snapshot().State})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusAccepted, checkResponse{Queued: n})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Serve answers the API on the unix socket at path until ctx ends. A socket
// file an earlier run left behind is removed first; the new one is mode 0600,
// so only root - every daemon here runs as root - can ask.
func Serve(ctx context.Context, path string, src Source) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	_ = os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0600); err != nil {
		l.Close()
		return err
	}
	srv := &http.Server{Handler: NewHandler(src), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	if err := srv.Serve(l); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
