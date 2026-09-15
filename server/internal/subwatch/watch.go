package subwatch

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

const (
	ProbeInterval      = 30 * time.Second
	DeadAfter          = 3 * time.Minute
	ImportRetry        = 5 * time.Minute
	SettleAfterRestart = 3 * time.Second
	defaultSOCKSPort   = 12346
)

const (
	msgMoved    = "Xray outbound is down; LAN clients moved to %s"
	msgNoTunnel = "Xray outbound is down; no Tunnel Director fallback"
)

type noteKind int

const (
	noteNone noteKind = iota
	noteMoved
	noteNoTunnel
)

type Watch struct {
	LoadVPN      func() (*vpnconfig.VPNDirectorConfig, error)
	LoadPlatform func() (vpnconfig.PlatformInfo, error)
	UpdateVPN    func(func(*vpnconfig.VPNDirectorConfig) error) error
	Apply        func() error
	RestartXray  func() error
	SaveServers  func([]vpnconfig.Server) error
	Generate     func(vpnconfig.Server) (generated bool, err error)
	Probe        func(ctx context.Context, socksPort int) error
	Fetch        func(ctx context.Context, url string) ([]vpnconfig.Server, error)
	Notify       func(msg string)
	Now          func() time.Time
	AfterRestart func(time.Duration)

	mu           sync.Mutex
	failSince    time.Time // zero => last probe succeeded
	lastImport   time.Time
	lastNote     string
	lastNoteKind noteKind
	running      bool
}

func (w *Watch) Start(ctx context.Context) {
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		return
	}
	w.running = true
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.running = false
		w.mu.Unlock()
	}()

	ticker := time.NewTicker(ProbeInterval)
	defer ticker.Stop()
	w.Tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.Tick(ctx)
		}
	}
}

func (w *Watch) Tick(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.applyDefaults()

	if w.LoadVPN == nil {
		return
	}
	cfg, err := w.LoadVPN()
	if err != nil {
		return
	}
	if !vpnconfig.Armed(cfg) {
		return
	}
	if cfg.Xray.Failover != nil {
		return
	}

	_, socks := vpnconfig.XrayInboundPorts(cfg)
	if socks == 0 {
		socks = defaultSOCKSPort
	}
	if err := w.Probe(ctx, socks); err == nil {
		w.failSince = time.Time{}
		w.lastNote = ""
		w.lastNoteKind = noteNone
		return
	}

	now := w.Now()
	if w.failSince.IsZero() {
		w.failSince = now
	}
	if now.Sub(w.failSince) < DeadAfter {
		return
	}

	var plat vpnconfig.PlatformInfo
	if w.LoadPlatform != nil {
		if p, err := w.LoadPlatform(); err == nil {
			plat = p
		}
	}
	id := vpnconfig.FirstTDExit(cfg, plat)
	if id == "" {
		w.notify(noteNoTunnel, msgNoTunnel)
		return
	}
	if w.UpdateVPN == nil {
		return
	}
	if err := w.UpdateVPN(func(current *vpnconfig.VPNDirectorConfig) error {
		vpnconfig.MoveXrayClientsToTunnel(current, id)
		return nil
	}); err != nil {
		return
	}
	if w.Apply != nil {
		if err := w.Apply(); err != nil {
			return
		}
	}
	w.notify(noteMoved, fmt.Sprintf(msgMoved, "tunnel:"+id))
}

func (w *Watch) applyDefaults() {
	if w.Now == nil {
		w.Now = time.Now
	}
	if w.AfterRestart == nil {
		w.AfterRestart = time.Sleep
	}
	if w.Probe == nil {
		w.Probe = func(ctx context.Context, port int) error {
			return ProbeSOCKS(ctx, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), ProbeURL)
		}
	}
}

func (w *Watch) notify(kind noteKind, msg string) {
	if w.lastNoteKind == kind {
		return
	}
	w.lastNoteKind = kind
	w.lastNote = msg
	if w.Notify != nil {
		w.Notify(msg)
	}
}
