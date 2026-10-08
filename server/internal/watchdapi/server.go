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

// NewHandler serves the monitor and an optional watch/notification source.
func NewHandler(src Source, automation ...AutomationSource) http.Handler {
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
	if len(automation) > 0 && automation[0] != nil {
		addNotificationHandlers(mux, automation[0])
	}
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

func (l *instanceListener) Addr() net.Addr {
	return &net.UnixAddr{Name: l.path, Net: "unix"}
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
	return listenWithPublication(ctx, path, dial, os.Chmod, os.Link)
}

func listenWithPublication(ctx context.Context, path string, dial func(context.Context, string, string) (net.Conn, error), chmod func(string, os.FileMode) error, link func(string, string) error) (net.Listener, error) {
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
	// A shortened private bind must still leave the public address dialable.
	if len(path) >= len(syscall.RawSockaddrUnix{}.Path) {
		return nil, &net.OpError{Op: "listen", Net: "unix", Addr: &net.UnixAddr{Name: path, Net: "unix"}, Err: syscall.EINVAL}
	}
	listener, info, err := publishSocket(ctx, path, chmod, link)
	if err != nil {
		return nil, err
	}
	owned = true
	return &instanceListener{Listener: listener, lock: lock, path: path, info: info}, nil
}

func removeOwnedSocket(path string, owned os.FileInfo) error {
	if owned == nil {
		return nil
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket != 0 && os.SameFile(info, owned) {
		return os.Remove(path)
	}
	return nil
}

func publishSocket(ctx context.Context, path string, chmod func(string, os.FileMode) error, link func(string, string) error) (listener *net.UnixListener, info os.FileInfo, err error) {
	dir, err := os.MkdirTemp(filepath.Dir(path), ".watchd-")
	if err != nil {
		return nil, nil, err
	}
	var fd *os.File
	var bindPath string
	dirInfo, err := os.Lstat(dir)
	defer func() {
		if err != nil && listener != nil {
			listener.Close()
		}
		err = errors.Join(err, removeOwnedSocket(bindPath, info))
		if fd != nil {
			err = errors.Join(err, fd.Close())
		}
		if current, statErr := os.Lstat(dir); statErr == nil && dirInfo != nil && os.SameFile(current, dirInfo) {
			// Remove only this invocation's empty private directory.
			err = errors.Join(err, os.Remove(dir))
		}
		if err != nil && listener != nil {
			listener.Close()
			err = errors.Join(err, removeOwnedSocket(path, info))
			listener = nil
		}
	}()
	if err != nil {
		return nil, nil, err
	}
	if err = os.Chmod(dir, 0700); err != nil {
		return nil, nil, err
	}
	fd, err = os.Open(dir)
	if err != nil {
		return nil, nil, err
	}
	current, err := fd.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !os.SameFile(current, dirInfo) || current.Mode().Perm() != 0700 {
		return nil, nil, errors.New("monitor private directory changed")
	}
	// Linux resolves this short address into the owned directory on the same
	// filesystem, even when the final address leaves no room for a suffix.
	bindPath = fmt.Sprintf("/proc/self/fd/%d/s", fd.Fd())
	listener, err = net.ListenUnix("unix", &net.UnixAddr{Name: bindPath, Net: "unix"})
	if err != nil {
		return nil, nil, err
	}
	listener.SetUnlinkOnClose(false)
	info, err = os.Lstat(bindPath)
	if err != nil {
		return listener, info, err
	}
	staged := filepath.Join(dir, "s")
	if err = ctx.Err(); err != nil {
		return listener, info, err
	}
	if err = chmod(staged, 0600); err != nil {
		return listener, info, err
	}
	current, err = os.Lstat(staged)
	if err != nil {
		return listener, info, err
	}
	if current.Mode()&os.ModeSocket == 0 || !os.SameFile(current, info) {
		return listener, info, errors.New("monitor private socket changed")
	}
	if current.Mode().Perm() != 0600 {
		return listener, info, fmt.Errorf("monitor socket permissions: %w", os.ErrPermission)
	}
	if err = ctx.Err(); err != nil {
		return listener, info, err
	}
	// Hard-link publication is atomic and refuses every existing destination.
	if err = link(staged, path); err != nil {
		return listener, info, err
	}
	current, err = os.Lstat(path)
	if err != nil {
		return listener, info, err
	}
	if current.Mode()&os.ModeSocket == 0 || !os.SameFile(current, info) || current.Mode().Perm() != 0600 {
		return listener, info, errors.New("monitor published socket changed")
	}
	if err = ctx.Err(); err != nil {
		return listener, info, err
	}
	return listener, info, nil
}

// ServeListener serves an acquired listener. Its caller retains the lifetime
// lock until both serving and the monitor's prober/state shutdown have ended.
func ServeListener(ctx context.Context, l net.Listener, src Source, automation ...AutomationSource) error {
	if owned, ok := l.(*instanceListener); ok {
		l = owned.Listener
	}
	srv := &http.Server{Handler: NewHandler(src, automation...), ReadHeaderTimeout: 5 * time.Second}
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
