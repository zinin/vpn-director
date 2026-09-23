// internal/service/config.go
package service

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/zinin/vpn-director/server/internal/paths"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

const (
	configLockTimeout = 30 * time.Second
	configLockPoll    = 50 * time.Millisecond
)

// ConfigService handles vpn-director configuration operations
type ConfigService struct {
	scriptsDir     string
	defaultDataDir string
	configPath     string
	lockTimeout    time.Duration // how long UpdateVPNConfig waits for the lock
	lockPoll       time.Duration // interval between LOCK_NB attempts
}

// Compile-time check that ConfigService implements ConfigStore
var _ ConfigStore = (*ConfigService)(nil)

// NewConfigService creates a new ConfigService. The optional configPath names
// the config file itself, for callers that take one from a --config flag; by
// default it is vpn-director.json inside scriptsDir.
func NewConfigService(scriptsDir, defaultDataDir string, configPath ...string) *ConfigService {
	s := &ConfigService{
		scriptsDir:     scriptsDir,
		defaultDataDir: defaultDataDir,
		configPath:     filepath.Join(scriptsDir, "vpn-director.json"),
		lockTimeout:    configLockTimeout,
		lockPoll:       configLockPoll,
	}
	if len(configPath) > 0 && configPath[0] != "" {
		s.configPath = configPath[0]
	}
	return s
}

// LockPath returns the lock file guarding vpn-director.json. It lives next to
// the config, so dev mode locks inside testdata/dev. /var/lock/vpn-director.lock
// remains the shell script's own lock and Go never touches it.
func (s *ConfigService) LockPath() string {
	dir, base := filepath.Split(s.configPath)
	return filepath.Join(dir, "."+base+".lock")
}

// ConfigPath returns the path to vpn-director.json
func (s *ConfigService) ConfigPath() string {
	return s.configPath
}

// ScriptsDir returns the scripts directory
func (s *ConfigService) ScriptsDir() string {
	return s.scriptsDir
}

// DataDir returns the data directory from config (no caching, returns error)
func (s *ConfigService) DataDir() (string, error) {
	cfg, err := s.LoadVPNConfig()
	if err != nil {
		return "", err
	}
	if cfg.DataDir != "" {
		// Relative means "beside vpn-director.json". It cannot mean "beside
		// the directory this daemon was started in": production daemons move
		// to / at startup, and the directory before that was whatever the
		// launcher had - after a self-update, one that has just been deleted.
		return paths.Resolve(filepath.Dir(s.configPath), cfg.DataDir), nil
	}
	return s.defaultDataDir, nil
}

// DataDirOrDefault returns data directory, falling back to default on error
// Used by /import when vpn-director.json may not exist
func (s *ConfigService) DataDirOrDefault() string {
	dataDir, err := s.DataDir()
	if err != nil || dataDir == "" {
		return s.defaultDataDir
	}
	return dataDir
}

// LoadVPNConfig loads the VPN Director configuration
func (s *ConfigService) LoadVPNConfig() (*vpnconfig.VPNDirectorConfig, error) {
	return vpnconfig.LoadVPNDirectorConfig(s.ConfigPath())
}

// saveVPNConfig writes the VPN Director configuration; callers hold the config lock.
func (s *ConfigService) saveVPNConfig(cfg *vpnconfig.VPNDirectorConfig) error {
	return vpnconfig.SaveVPNDirectorConfig(s.ConfigPath(), cfg)
}

// UpdateVPNConfig implements ConfigStore; see the interface for the contract.
func (s *ConfigService) UpdateVPNConfig(fn func(cfg *vpnconfig.VPNDirectorConfig) error) error {
	unlock, err := s.lockConfig()
	if err != nil {
		return err
	}
	defer unlock()

	cfg, err := s.LoadVPNConfig()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrConfigLoad, err)
	}
	if err := fn(cfg); err != nil {
		return err
	}
	if err := s.saveVPNConfig(cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	return nil
}

// lockConfig takes an exclusive flock on LockPath, polling LOCK_NB every
// lockPoll for up to lockTimeout. flock locks belong to the open file
// description, so two ConfigService values in one process contend exactly
// like the bot and the Web UI do across processes.
func (s *ConfigService) lockConfig() (unlock func(), err error) {
	f, err := os.OpenFile(s.LockPath(), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open config lock: %w", err)
	}
	deadline := time.Now().Add(s.lockTimeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, fmt.Errorf("lock config: %w", err)
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, ErrConfigLockTimeout
		}
		time.Sleep(s.lockPoll)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// SubscriptionsDir is where the data directory keeps the subscription files.
func (s *ConfigService) SubscriptionsDir() (string, error) {
	dataDir, err := s.DataDir()
	if err != nil {
		return "", err
	}
	return vpnconfig.SubscriptionsDir(dataDir), nil
}

// LoadSubscriptions reads every subscription, in order (vpnconfig.LoadSubscriptions).
func (s *ConfigService) LoadSubscriptions() ([]vpnconfig.Subscription, error) {
	dir, err := s.SubscriptionsDir()
	if err != nil {
		return nil, err
	}
	return vpnconfig.LoadSubscriptions(dir)
}

// SaveSubscription writes one subscription file. The caller holds the config
// lock. It also takes away the servers.json of earlier releases.
func (s *ConfigService) SaveSubscription(sub vpnconfig.Subscription) error {
	dataDir, err := s.DataDir()
	if err != nil {
		return err
	}
	if err := vpnconfig.SaveSubscription(vpnconfig.SubscriptionsDir(dataDir), sub); err != nil {
		return err
	}
	s.removeLegacyServers(dataDir)
	return nil
}

// DeleteSubscription removes one subscription file. The caller holds the
// config lock. It also takes away the servers.json of earlier releases.
func (s *ConfigService) DeleteSubscription(id string) error {
	dataDir, err := s.DataDir()
	if err != nil {
		return err
	}
	if err := vpnconfig.DeleteSubscriptionFile(vpnconfig.SubscriptionsDir(dataDir), id); err != nil {
		return err
	}
	s.removeLegacyServers(dataDir)
	return nil
}

// removeLegacyServers is best effort: the subscription write it follows has
// happened, and a servers.json left behind is read by nobody.
func (s *ConfigService) removeLegacyServers(dataDir string) {
	if err := vpnconfig.RemoveLegacyServers(dataDir); err != nil {
		slog.Warn("Failed to remove the servers.json of an earlier release", "error", err)
	}
}

// LoadServers is every server of every subscription, in subscription order,
// each carrying its subscription's id (vpnconfig.AllServers).
func (s *ConfigService) LoadServers() ([]vpnconfig.Server, error) {
	subs, err := s.LoadSubscriptions()
	if err != nil {
		return nil, err
	}
	return vpnconfig.AllServers(subs), nil
}

// SaveServers saves the servers list (creates directory if needed)
// Uses DataDirOrDefault() to allow saving even without vpn-director.json
func (s *ConfigService) SaveServers(servers []vpnconfig.Server) error {
	dataDir := s.DataDirOrDefault()
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return err
	}
	return vpnconfig.SaveServers(filepath.Join(dataDir, "servers.json"), servers)
}
