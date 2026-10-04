package watchdapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
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
		req := &checkRequest{}
		body := http.MaxBytesReader(w, r.Body, 1<<20)
		// An empty body is a check of every endpoint, like {}.
		decoder := json.NewDecoder(body)
		err := decoder.Decode(&req)
		if err == nil {
			var trailing any
			if !errors.Is(decoder.Decode(&trailing), io.EOF) {
				writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid request body"})
				return
			}
		}
		if req == nil || (err != nil && !errors.Is(err, io.EOF)) {
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

const socketProbeTimeout = 250 * time.Millisecond

type instanceListener struct {
	net.Listener
	lock     *os.File
	path     string
	info     os.FileInfo
	once     sync.Once
	closeErr error
}

func (l *instanceListener) Close() error {
	l.once.Do(func() {
		l.closeErr = l.Listener.Close()
		if errors.Is(l.closeErr, net.ErrClosed) {
			l.closeErr = nil
		}
		if info, err := os.Lstat(l.path); err == nil && info.Mode()&os.ModeSocket != 0 && os.SameFile(info, l.info) {
			if err := os.Remove(l.path); l.closeErr == nil {
				l.closeErr = err
			}
		}
		if err := l.lock.Close(); l.closeErr == nil {
			l.closeErr = err
		}
	})
	return l.closeErr
}

// Listen acquires lifetime ownership of path and its mode-0600 listener.
// The caller closes it after the monitor's prober/state shutdown to release
// ownership. The lock file stays in place so every instance locks one inode.
func Listen(ctx context.Context, path string) (net.Listener, error) {
	return listen(ctx, path, (&net.Dialer{}).DialContext)
}

func listen(ctx context.Context, path string, dial func(context.Context, string, string) (net.Conn, error)) (net.Listener, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	owned := false
	defer func() {
		if !owned {
			lock.Close()
		}
	}()
	lockInfo, err := lock.Stat()
	if err != nil {
		return nil, err
	}
	if !lockInfo.Mode().IsRegular() {
		return nil, errors.New("monitor lock is not a regular file")
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, fmt.Errorf("lock monitor instance: %w", err)
	}
	currentLock, err := os.Lstat(path + ".lock")
	if err != nil {
		return nil, err
	}
	if !os.SameFile(lockInfo, currentLock) {
		return nil, errors.New("monitor lock file changed")
	}
	info, err := os.Lstat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("monitor socket path is not a Unix socket")
		}
		probeCtx, cancel := context.WithTimeout(ctx, socketProbeTimeout)
		conn, probeErr := dial(probeCtx, "unix", path)
		cancel()
		if probeErr == nil {
			conn.Close()
			return nil, errors.New("monitor socket is accepting connections")
		}
		if !errors.Is(probeErr, syscall.ECONNREFUSED) {
			return nil, probeErr
		}
		current, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if current.Mode()&os.ModeSocket == 0 || !os.SameFile(info, current) {
			return nil, errors.New("monitor socket path changed")
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	listener.SetUnlinkOnClose(false)
	info, err = os.Lstat(path)
	if err != nil {
		listener.Close()
		return nil, err
	}
	if info.Mode()&os.ModeSocket == 0 {
		listener.Close()
		return nil, errors.New("monitor socket path changed")
	}
	l := &instanceListener{Listener: listener, lock: lock, path: path, info: info}
	owned = true
	if err := os.Chmod(path, 0600); err != nil {
		l.Close()
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// Serve answers the API on an owned unix socket until ctx ends.
func Serve(ctx context.Context, path string, src Source) error {
	l, err := Listen(ctx, path)
	if err != nil {
		return err
	}
	defer l.Close()
	return ServeListener(ctx, l, src)
}

// ServeListener serves an acquired listener. Its caller retains the lifetime
// lock until both serving and the monitor's prober/state shutdown have ended.
func ServeListener(ctx context.Context, l net.Listener, src Source) error {
	if owned, ok := l.(*instanceListener); ok {
		l = owned.Listener
	}
	srv := &http.Server{Handler: NewHandler(src), ReadHeaderTimeout: 5 * time.Second}
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		select {
		case <-ctx.Done():
			srv.Close()
		case <-done:
		}
	}()
	defer func() { close(done); <-stopped }()
	if err := srv.Serve(l); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
