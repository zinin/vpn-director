package monitor

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

func TestSettingsFrom_NoSectionIsTheDefaults(t *testing.T) {
	s, warns := SettingsFrom(nil)
	want := Settings{Enabled: true, Interval: time.Minute, DeadMax: 30 * time.Minute, Concurrency: 8}
	if s != want || len(warns) != 0 {
		t.Fatalf("settings %+v, warnings %v", s, warns)
	}
}

func TestSettingsFrom_ReadsEveryKey(t *testing.T) {
	off := false
	s, warns := SettingsFrom(&vpnconfig.MonitorConfig{Enabled: &off, Interval: "2m", DeadIntervalMax: "1h", Concurrency: 4, LogLevel: "debug"})
	want := Settings{Enabled: false, Interval: 2 * time.Minute, DeadMax: time.Hour, Concurrency: 4, LogLevel: "debug"}
	if s != want || len(warns) != 0 {
		t.Fatalf("settings %+v, warnings %v", s, warns)
	}
}

// A value out of bounds takes its default and says so; it never stops the
// monitor.
func TestSettingsFrom_AValueOutOfBoundsTakesItsDefault(t *testing.T) {
	s, warns := SettingsFrom(&vpnconfig.MonitorConfig{Interval: "5s", DeadIntervalMax: "soon", Concurrency: 100})
	want := Settings{Enabled: true, Interval: time.Minute, DeadMax: 30 * time.Minute, Concurrency: 8}
	if s != want {
		t.Fatalf("settings %+v", s)
	}
	if len(warns) != 3 || !strings.Contains(warns[0], "monitor.interval") || !strings.Contains(warns[1], "monitor.dead_interval_max") || !strings.Contains(warns[2], "monitor.concurrency") {
		t.Fatalf("warnings %v", warns)
	}
}

// A dead endpoint's first pause is twice the interval, so the cap is never
// below that, even when the default cap is.
func TestSettingsFrom_TheCapIsAtLeastTwiceTheInterval(t *testing.T) {
	s, _ := SettingsFrom(&vpnconfig.MonitorConfig{Interval: "20m"})
	if s.DeadMax != 40*time.Minute {
		t.Fatalf("cap %s, want 40m", s.DeadMax)
	}
	s, warns := SettingsFrom(&vpnconfig.MonitorConfig{Interval: "20m", DeadIntervalMax: "30m"})
	if s.DeadMax != 40*time.Minute || len(warns) != 1 {
		t.Fatalf("cap %s, warnings %v", s.DeadMax, warns)
	}
}

func TestSettingsFrom_AnEmptySectionIsTheDefaults(t *testing.T) {
	s, warns := SettingsFrom(&vpnconfig.MonitorConfig{})
	want := Settings{Enabled: true, Interval: time.Minute, DeadMax: 30 * time.Minute, Concurrency: 8}
	if s != want || len(warns) != 0 {
		t.Fatalf("settings %+v, warnings %v", s, warns)
	}
}

func TestSettingsFrom_ExplicitJSONConcurrencyZeroWarns(t *testing.T) {
	var c vpnconfig.MonitorConfig
	if err := json.Unmarshal([]byte(`{"concurrency":0}`), &c); err != nil {
		t.Fatal(err)
	}
	s, warns := SettingsFrom(&c)
	want := Settings{Enabled: true, Interval: time.Minute, DeadMax: 30 * time.Minute, Concurrency: 8}
	if s != want || len(warns) != 1 || !strings.Contains(warns[0], "monitor.concurrency 0") || !strings.Contains(warns[0], "using 8") {
		t.Fatalf("settings %+v, warnings %v", s, warns)
	}
}

func TestSettingsFrom_JSONConcurrency(t *testing.T) {
	cases := []struct {
		name        string
		data        string
		concurrency int
		warnings    int
	}{
		{name: "absent", data: `{}`, concurrency: 8},
		{name: "negative", data: `{"concurrency":-1}`, concurrency: 8, warnings: 1},
		{name: "minimum", data: `{"concurrency":1}`, concurrency: 1},
		{name: "maximum", data: `{"concurrency":32}`, concurrency: 32},
		{name: "over maximum", data: `{"concurrency":33}`, concurrency: 8, warnings: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var c vpnconfig.MonitorConfig
			if err := json.Unmarshal([]byte(tc.data), &c); err != nil {
				t.Fatal(err)
			}
			s, warns := SettingsFrom(&c)
			if s.Concurrency != tc.concurrency || len(warns) != tc.warnings {
				t.Fatalf("settings %+v, warnings %v", s, warns)
			}
			if len(warns) != 0 && !strings.Contains(warns[0], "monitor.concurrency") {
				t.Fatalf("warnings %v", warns)
			}
		})
	}
}

func TestSettingsFrom_IntervalMustLeaveRoomForTheFirstDeadPause(t *testing.T) {
	const largest = time.Duration(1<<63 - 1)
	for _, d := range []time.Duration{largest/2 + 1, largest} {
		s, warns := SettingsFrom(&vpnconfig.MonitorConfig{Interval: d.String()})
		if s.Interval != DefaultInterval || s.DeadMax != DefaultDeadMax || len(warns) != 1 || !strings.Contains(warns[0], "monitor.interval") {
			t.Fatalf("unrepresentable first pause: interval=%s settings=%+v warnings=%v", d, s, warns)
		}
	}
	s, warns := SettingsFrom(&vpnconfig.MonitorConfig{Interval: (largest / 2).String(), DeadIntervalMax: largest.String()})
	if len(warns) != 0 || s.Interval != largest/2 || s.DeadMax != largest {
		t.Fatalf("boundary settings=%+v warnings=%v", s, warns)
	}
}

func TestSubscriptionRefreshFrom(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  time.Duration
		warns bool
	}{
		{"", 5 * time.Minute, false},
		{"7m", 7 * time.Minute, false},
		{"1m", time.Minute, false},
		{"0", 0, false},
		{"0s", 0, false},
		{"30s", 5 * time.Minute, true},
		{"-5m", 5 * time.Minute, true},
		{"soon", 5 * time.Minute, true},
	} {
		got, warn := SubscriptionRefreshFrom(&vpnconfig.MonitorConfig{SubscriptionRefresh: tc.value})
		if got != tc.want || (warn != "") != tc.warns {
			t.Errorf("%q: %s, warning %q", tc.value, got, warn)
		}
	}
	if got, warn := SubscriptionRefreshFrom(nil); got != DefaultSubscriptionRefresh || warn != "" {
		t.Fatalf("no section: %s, warning %q", got, warn)
	}
}

// The monitor's settings reader logs each distinct set of warnings once; the
// refresh period's warning travels with them.
func TestSettingsFrom_WarnsAboutASubscriptionRefreshOutOfBounds(t *testing.T) {
	_, warns := SettingsFrom(&vpnconfig.MonitorConfig{SubscriptionRefresh: "30s"})
	if len(warns) != 1 || !strings.Contains(warns[0], "monitor.subscription_refresh") || !strings.Contains(warns[0], "using 5m0s") {
		t.Fatalf("warnings %v", warns)
	}
}
