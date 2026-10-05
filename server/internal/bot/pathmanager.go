package bot

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/zinin/vpn-director/server/internal/netpath"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

const defaultPathInterval = 30 * time.Second

const defaultAPIBase = "https://api.telegram.org"

// newFailureCycleContext starts the context for a ReportFailure reselect.
// No overall deadline: each probePath call caps itself at probeTimeout so a
// later live tunnel is still tried after earlier timeouts.
var newFailureCycleContext = func() (context.Context, context.CancelFunc) {
	return context.Background(), func() {}
}

var (
	_ PathSource = (*PathManager)(nil)
	_ IdleCloser = (*PathManager)(nil)
)

type PathManager struct {
	token         string
	loadVPN       func() (*vpnconfig.VPNDirectorConfig, error)
	loadPlatform  func() (vpnconfig.PlatformInfo, error)
	listening     func(port int) bool
	probe         func(ctx context.Context, p Path) error
	loadTunnelIdx func() map[string]int
	apiBase       string
	interval      time.Duration

	mu      sync.Mutex
	current Path
	cycling bool
	running bool
	closers []func(Path)
	// lastNoPath is the tried set already warned about, empty after a cycle that found a path.
	lastNoPath string
}

type PathManagerConfig struct {
	Token         string
	LoadVPN       func() (*vpnconfig.VPNDirectorConfig, error)
	LoadPlatform  func() (vpnconfig.PlatformInfo, error)
	Listening     func(port int) bool                     // nil => socksListening
	Probe         func(ctx context.Context, p Path) error // nil => probePath(ctx, apiBase, token, p)
	LoadTunnelIdx func() map[string]int                   // nil => TUN_DIR_TABLES
	APIBase       string                                  // empty => https://api.telegram.org
	Interval      time.Duration                           // 0 => 30s
}

func NewPathManager(cfg PathManagerConfig) *PathManager {
	m := &PathManager{
		token:         cfg.Token,
		loadVPN:       cfg.LoadVPN,
		loadPlatform:  cfg.LoadPlatform,
		listening:     cfg.Listening,
		probe:         cfg.Probe,
		loadTunnelIdx: cfg.LoadTunnelIdx,
		apiBase:       cfg.APIBase,
		interval:      cfg.Interval,
	}
	if m.listening == nil {
		m.listening = socksListening
	}
	if m.apiBase == "" {
		m.apiBase = defaultAPIBase
	}
	if m.interval == 0 {
		m.interval = defaultPathInterval
	}
	if m.probe == nil {
		m.probe = func(ctx context.Context, p Path) error {
			return probePath(ctx, m.apiBase, m.token, p)
		}
	}
	if m.loadTunnelIdx == nil {
		m.loadTunnelIdx = func() map[string]int {
			return netpath.LoadTunnelIndexes(defaultTunnelTablesPath)
		}
	}
	return m
}

func (m *PathManager) Current() Path {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current
}

func (m *PathManager) RegisterIdleCloser(fn func(Path)) {
	m.mu.Lock()
	m.closers = append(m.closers, fn)
	m.mu.Unlock()
}

// ReportFailure starts a reselect when the failing path is still the current one.
// A failure for a path that has since been retired is a no-op, so a late error on
// an old keep-alive cannot flap the path that replaced it.
//
// A failure that arrives while a cycle is running is dropped rather than queued,
// which is safe because the caller retries: tgbotapi's GetUpdatesChan loop
// re-issues the request after every failure — a 3-second pause in v5.5.1 — so
// while a path is dead the next failure lands within seconds of the running cycle
// finishing and starts a fresh one. The drop costs one retry interval, not the
// 30-second tick.
//
// There is deliberately no minimum interval between failure-driven cycles:
// m.cycling already keeps cycles from overlapping, so the rate is bounded by the
// duration of a cycle, which is at least one HTTPS probe — and a cycle that ends
// on a live direct is exactly one probe.
func (m *PathManager) ReportFailure(p Path) {
	m.mu.Lock()
	skip := !p.Same(m.current) || m.cycling
	m.mu.Unlock()
	if skip {
		return
	}
	go func() {
		ctx, cancel := newFailureCycleContext()
		defer cancel()
		m.SelectOnce(ctx)
	}()
}

func (m *PathManager) Start(ctx context.Context) {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return
	}
	m.running = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.running = false
		m.mu.Unlock()
	}()
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.SelectOnce(ctx)
		}
	}
}

func (m *PathManager) SelectOnce(ctx context.Context) {
	m.mu.Lock()
	if m.cycling {
		m.mu.Unlock()
		return
	}
	m.cycling = true
	current := m.current
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.cycling = false
		m.mu.Unlock()
	}()

	stored := current

	var tried []string
	direct := Path{Kind: netpath.KindDirect}
	directLive := m.probeOne(ctx, direct, &tried)

	// Discovery — the config read, the fork of "vpn-director.sh platform", the
	// SOCKS listen check and the TUN_DIR_TABLES read — only feeds the backups,
	// so a healthy direct path costs one probe and nothing else.
	currentLive := false
	var replacement Path
	if !directLive {
		var cfg *vpnconfig.VPNDirectorConfig
		if m.loadVPN != nil {
			loaded, err := m.loadVPN()
			if err != nil {
				slog.Warn("Failed to load VPN Director config, no tunnel candidates", "error", err)
				cfg = nil
			} else {
				cfg = loaded
			}
		}

		var plat vpnconfig.PlatformInfo
		if m.loadPlatform != nil {
			loaded, err := m.loadPlatform()
			if err != nil {
				slog.Warn("Failed to read platform info, no tunnel candidates", "error", err)
				plat = vpnconfig.PlatformInfo{}
			} else {
				plat = loaded
			}
		}

		port := netpath.SOCKSPort(cfg)
		socksUp := m.listening(port)
		idxByID := m.loadTunnelIdx()
		cands := netpath.Candidates(cfg, plat, socksUp, idxByID)

		currentProbed := false
		if current.Kind == netpath.KindSOCKS || current.Kind == netpath.KindTunnel {
			if fresh, ok := matchPath(cands, current); ok {
				current = fresh
				currentLive = m.probeOne(ctx, current, &tried)
				currentProbed = true
			}
		}

		if current.Kind == netpath.KindNone || !currentLive {
			for _, p := range cands {
				if p.Kind == netpath.KindDirect {
					continue
				}
				if currentProbed && p.Same(current) {
					continue
				}
				if m.probeOne(ctx, p, &tried) {
					replacement = p
					break
				}
			}
		}
	}

	next := selectPath(current, directLive, currentLive, replacement)
	changed := !next.Same(stored)
	if changed {
		slog.Info("Telegram API path selected", "from", stored.String(), "to", next.String())
		if stored.Kind == netpath.KindDirect && (next.Kind == netpath.KindSOCKS || next.Kind == netpath.KindTunnel) {
			slog.Warn("Telegram API unreachable on WAN, using backup path")
		}
	}
	if changed || !next.ParamsEqual(stored) {
		m.mu.Lock()
		closers := slices.Clone(m.closers)
		m.mu.Unlock()
		for _, fn := range closers {
			fn(next)
		}
		m.mu.Lock()
		m.current = next
		m.mu.Unlock()
	}
	if next.Kind == netpath.KindNone {
		key := strings.Join(tried, ",")
		m.mu.Lock()
		repeat := key == m.lastNoPath
		m.lastNoPath = key
		m.mu.Unlock()
		if repeat {
			slog.Debug("Telegram API still unreachable on every path", "tried", tried)
		} else {
			slog.Warn("Telegram API unreachable on every path", "tried", tried)
		}
	} else {
		m.mu.Lock()
		m.lastNoPath = ""
		m.mu.Unlock()
	}
}

func (m *PathManager) probeOne(ctx context.Context, p Path, tried *[]string) bool {
	err := m.probe(ctx, p)
	live := err == nil
	slog.Debug("Telegram API path probe", "path", p.String(), "live", live)
	reason := "live"
	if !live {
		reason = pathFailReason(err)
	}
	*tried = append(*tried, p.String()+"="+reason)
	return live
}

func matchPath(cands []Path, p Path) (Path, bool) {
	for _, c := range cands {
		if c.Same(p) {
			return c, true
		}
	}
	return Path{}, false
}

func pathFailReason(err error) string {
	if err == nil {
		return "live"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	return "dead"
}
