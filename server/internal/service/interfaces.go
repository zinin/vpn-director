// internal/service/interfaces.go
package service

import (
	"context"
	"errors"

	"github.com/zinin/vpn-director/server/internal/shell"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// ConfigStore, VPNDirector, XrayGenerator, NetworkInfo, LogReader interfaces are defined here
// in service/ rather than handler/ so both handler/ and wizard/ can
// import them without coupling handler <-> wizard.
//
// TODO: If the number of consumers grows or interfaces become complex,
// consider extracting to internal/contract/ for cleaner separation.

// ShellExecutor is the interface for executing shell commands. ctx bounds the
// command: services build it from context.Background() with their own
// per-command timeout, never from an HTTP request, so a closed browser tab
// cannot kill an apply half-way through its iptables changes.
type ShellExecutor interface {
	Exec(ctx context.Context, name string, args ...string) (*shell.Result, error)
}

// ErrConfigLoad marks an UpdateVPNConfig failure that happened before fn ran:
// vpn-director.json could not be read and nothing was changed. The cause is
// wrapped as well, so errors.Is(err, os.ErrNotExist) still works.
var ErrConfigLoad = errors.New("load config")

// ErrConfigLockTimeout is returned when another process held the config lock
// for the whole wait; nothing was read or written.
var ErrConfigLockTimeout = errors.New("config lock timeout")

// ConfigStore is the interface for config operations
type ConfigStore interface {
	LoadVPNConfig() (*vpnconfig.VPNDirectorConfig, error)
	LoadServers() ([]vpnconfig.Server, error)
	// LoadSubscriptions reads every subscription file, ordered by when it was
	// added; LoadServers is their servers, flattened in that order.
	LoadSubscriptions() ([]vpnconfig.Subscription, error)
	// SaveSubscription and DeleteSubscription write one subscription file.
	// Call them only inside UpdateVPNConfig, so every write to the files
	// happens under the config lock - vpnconfig's subscription operations do.
	SaveSubscription(vpnconfig.Subscription) error
	DeleteSubscription(id string) error
	// UpdateVPNConfig runs fn under an exclusive cross-process lock:
	// lock, load, fn, save, unlock. Readers stay lock-free because Save is
	// atomic. A load failure comes back wrapped in ErrConfigLoad, an error
	// from fn is returned as is and skips the save, and a lock held by
	// another process for the whole wait yields ErrConfigLockTimeout.
	//
	// There is deliberately no unlocked save method: every writer goes through
	// UpdateVPNConfig, so no code path can skip the lock.
	UpdateVPNConfig(fn func(cfg *vpnconfig.VPNDirectorConfig) error) error
	DataDir() (string, error)
	ScriptsDir() string
}

// VPNDirector is the interface for VPN Director operations
type VPNDirector interface {
	Status() (string, error)
	Apply() error
	Restart() error
	RestartXray() error
	Stop() error
	// Update downloads fresh ipsets and reapplies the configuration
	// (vpn-director.sh update). Apply reuses cached ipsets instead.
	Update() error
	// Platform runs `vpn-director.sh platform` and decodes it: the tunnels
	// the router has, by id. Live, uncached; the caller validates routes
	// against it at the moment of the change.
	Platform() (vpnconfig.PlatformInfo, error)
}

// XrayGenerator is the interface for Xray config generation. The optional
// ports keep the generated inbounds in step with advanced.xray.
type XrayGenerator interface {
	GenerateConfig(server vpnconfig.Server, ports ...InboundPorts) error
}

// NetworkInfo is the interface for network operations
type NetworkInfo interface {
	GetExternalIP() (string, error)
}

// LogReader is the interface for log reading
type LogReader interface {
	Read(path string, lines int) (string, error)
}

// defaultExecutor wraps shell.ExecContext
type defaultExecutor struct{}

func (e *defaultExecutor) Exec(ctx context.Context, name string, args ...string) (*shell.Result, error) {
	return shell.ExecContext(ctx, name, args...)
}

// DefaultExecutor returns the default shell executor
func DefaultExecutor() ShellExecutor {
	return &defaultExecutor{}
}
