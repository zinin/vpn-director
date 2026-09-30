// Package monitor checks, minute by minute, whether every server of every
// subscription carries traffic. A separate Xray process, the prober, holds one
// outbound per endpoint (one address of one server, as the walk dials it), and
// a check fetches ProbeURL through it. The live Xray is never touched.
package monitor

import (
	"fmt"
	"time"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// Defaults and bounds of the monitor section.
const (
	DefaultInterval    = time.Minute
	DefaultDeadMax     = 30 * time.Minute
	DefaultConcurrency = 8
	MinInterval        = 10 * time.Second
	MaxConcurrency     = 32
)

// Settings is the monitor section with its defaults filled in.
type Settings struct {
	Enabled bool
	// Interval is the pause between two checks of a live endpoint.
	Interval time.Duration
	// DeadMax caps the pause of a dead endpoint, which doubles from
	// 2 × Interval.
	DeadMax     time.Duration
	Concurrency int
	LogLevel    string
}

// SettingsFrom resolves the monitor section. A missing key takes its default,
// and so does a value out of bounds, with a warning naming it.
func SettingsFrom(c *vpnconfig.MonitorConfig) (Settings, []string) {
	s := Settings{Enabled: true, Interval: DefaultInterval, DeadMax: DefaultDeadMax, Concurrency: DefaultConcurrency}
	var warns []string
	if c == nil {
		return s, nil
	}
	if c.Enabled != nil {
		s.Enabled = *c.Enabled
	}
	s.LogLevel = c.LogLevel
	if c.Interval != "" {
		d, err := time.ParseDuration(c.Interval)
		if err != nil || d < MinInterval {
			warns = append(warns, fmt.Sprintf("monitor.interval %q is not a duration of at least %s; using %s", c.Interval, MinInterval, DefaultInterval))
		} else {
			s.Interval = d
		}
	}
	s.DeadMax = max(DefaultDeadMax, 2*s.Interval)
	if c.DeadIntervalMax != "" {
		d, err := time.ParseDuration(c.DeadIntervalMax)
		if err != nil || d < 2*s.Interval {
			warns = append(warns, fmt.Sprintf("monitor.dead_interval_max %q is not a duration of at least twice the interval; using %s", c.DeadIntervalMax, s.DeadMax))
		} else {
			s.DeadMax = d
		}
	}
	if c.HasConcurrency() {
		if c.Concurrency < 1 || c.Concurrency > MaxConcurrency {
			warns = append(warns, fmt.Sprintf("monitor.concurrency %d is not between 1 and %d; using %d", c.Concurrency, MaxConcurrency, DefaultConcurrency))
		} else {
			s.Concurrency = c.Concurrency
		}
	}
	return s, warns
}
