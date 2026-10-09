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
	// MaxInterval leaves room for the first dead pause.
	MaxInterval = time.Duration(1<<63-1) / 2
	// DefaultSubscriptionRefresh is how often watchd downloads every
	// subscription with a link when monitor.subscription_refresh is absent,
	// and MinSubscriptionRefresh the shortest period it takes: the monitor
	// reads the files once a minute anyway.
	DefaultSubscriptionRefresh = 5 * time.Minute
	MinSubscriptionRefresh     = time.Minute
)

// SubscriptionRefreshFrom resolves monitor.subscription_refresh, the period of
// watchd's periodic subscription refresh; 0 turns it off. A missing key is the
// default. A value that does not parse, is negative or is below
// MinSubscriptionRefresh is the default too, and the warning says so.
func SubscriptionRefreshFrom(c *vpnconfig.MonitorConfig) (time.Duration, string) {
	if c == nil || c.SubscriptionRefresh == "" {
		return DefaultSubscriptionRefresh, ""
	}
	d, err := time.ParseDuration(c.SubscriptionRefresh)
	if err == nil && (d == 0 || d >= MinSubscriptionRefresh) {
		return d, ""
	}
	return DefaultSubscriptionRefresh, fmt.Sprintf("monitor.subscription_refresh %q is neither 0 nor at least %s; using %s", c.SubscriptionRefresh, MinSubscriptionRefresh, DefaultSubscriptionRefresh)
}

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
		if err != nil || d < MinInterval || d > MaxInterval {
			warns = append(warns, fmt.Sprintf("monitor.interval %q is not between %s and %s; using %s", c.Interval, MinInterval, MaxInterval, DefaultInterval))
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
	// The refresh period is watchd's, not the engine's, but it lives in this
	// section: its warning is logged with the others.
	if _, warn := SubscriptionRefreshFrom(c); warn != "" {
		warns = append(warns, warn)
	}
	return s, warns
}
