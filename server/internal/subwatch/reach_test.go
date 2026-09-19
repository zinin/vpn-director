package subwatch

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

// captureLog sends slog to a buffer for the rest of the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func osloActiveCfg() *vpnconfig.VPNDirectorConfig {
	cfg := baseCfg()
	cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Name: "Oslo", Address: "oslo.example", Port: 443}
	return cfg
}

func osloServers() []vpnconfig.Server {
	return []vpnconfig.Server{{Name: "Oslo", Address: "oslo.example", Port: 443, IPs: []string{"203.0.113.10"}}}
}

// deadFake is a watch whose outbound fails every probe, with ovpnc2 to fall
// back on.
func deadFake() *fake {
	return &fake{cfg: osloActiveCfg(), plat: connected("ovpnc2"), probeErr: errProbe, now: time.Unix(1_700_000_000, 0)}
}

// reachWatch is f's watch with servers as servers.json and a TCP check that
// finds the addresses up names reachable.
func reachWatch(f *fake, servers []vpnconfig.Server, up map[string]bool) *Watch {
	w := f.watch()
	w.LoadServers = func() ([]vpnconfig.Server, error) { return servers, nil }
	w.Reachable = func(_ context.Context, ip string, _ int) bool { return up[ip] }
	return w
}

// assertDiesAt ticks every ProbeInterval from f.now, the first miss, and checks
// that the clients move on the tick d later and on none before it.
func assertDiesAt(t *testing.T, w *Watch, f *fake, d time.Duration) {
	t.Helper()
	start := f.now
	for f.now.Sub(start) < d {
		w.Tick(context.Background())
		if f.cfg.Xray.Failover != nil {
			t.Fatalf("moved %v after the first miss, want %v", f.now.Sub(start), d)
		}
		f.now = f.now.Add(ProbeInterval)
	}
	w.Tick(context.Background())
	if f.cfg.Xray.Failover == nil || f.cfg.Xray.Failover.Tunnel != "ovpnc2" {
		t.Fatalf("failover %+v %v after the first miss, want the move to ovpnc2", f.cfg.Xray.Failover, d)
	}
}

func TestDialIP(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    vpnconfig.Server
		want string
	}{
		{"resolved address", vpnconfig.Server{Address: "oslo.example", IPs: []string{"203.0.113.10"}}, "203.0.113.10"},
		{"empty entries skipped", vpnconfig.Server{Address: "oslo.example", IPs: []string{"", "203.0.113.11"}}, "203.0.113.11"},
		{"IPv4 literal address", vpnconfig.Server{Address: "203.0.113.12"}, "203.0.113.12"},
		{"hostname nothing resolved", vpnconfig.Server{Address: "oslo.example"}, ""},
		{"IPv6 only", vpnconfig.Server{Address: "2001:db8::1"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := dialIP(tc.s); got != tc.want {
				t.Fatalf("dialIP = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReachable_KeepsTheOrderOfTheCopiesThatAnswered(t *testing.T) {
	w := &Watch{Reachable: func(_ context.Context, ip string, _ int) bool { return ip != "203.0.113.1" }}
	copies := []vpnconfig.Server{
		{Name: "A", IPs: []string{"203.0.113.1"}},
		{Name: "B", IPs: []string{"203.0.113.2"}},
		{Name: "C", IPs: []string{"203.0.113.3"}},
	}
	var names []string
	for _, c := range w.reachable(context.Background(), copies) {
		names = append(names, c.Name)
	}
	if !reflect.DeepEqual(names, []string{"B", "C"}) {
		t.Fatalf("reachable %v, want [B C]", names)
	}
}

func TestTick_UnreachableServerIsDeadAfterAMinute(t *testing.T) {
	logs := captureLog(t)
	f := deadFake()
	assertDiesAt(t, reachWatch(f, osloServers(), map[string]bool{}), f, FastDeadAfter)
	if !strings.Contains(logs.String(), "reason=unreachable") {
		t.Fatalf("log %q, want the death's reason", logs.String())
	}
}

func TestTick_ReachableServerIsDeadAfterThreeMinutes(t *testing.T) {
	logs := captureLog(t)
	f := deadFake()
	assertDiesAt(t, reachWatch(f, osloServers(), map[string]bool{"203.0.113.10": true}), f, DeadAfter)
	if !strings.Contains(logs.String(), "reason=probe") {
		t.Fatalf("log %q, want the death's reason", logs.String())
	}
}

// One look that found the server up ends the streak for this run of misses.
func TestTick_OneReachableCheckKeepsThreeMinutes(t *testing.T) {
	f := deadFake()
	w := reachWatch(f, osloServers(), nil)
	checks := 0
	w.Reachable = func(context.Context, string, int) bool {
		checks++
		return checks == 2
	}
	assertDiesAt(t, w, f, DeadAfter)
}

func TestTick_NoReachAnswerKeepsThreeMinutes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(w *Watch)
	}{
		{"server not in servers.json", func(w *Watch) {
			w.LoadServers = func() ([]vpnconfig.Server, error) {
				return []vpnconfig.Server{{Name: "Paris", Address: "paris.example", Port: 443, IPs: []string{"203.0.113.30"}}}, nil
			}
		}},
		{"hostname nothing resolved", func(w *Watch) {
			w.LoadServers = func() ([]vpnconfig.Server, error) {
				return []vpnconfig.Server{{Name: "Oslo", Address: "oslo.example", Port: 443}}, nil
			}
		}},
		{"servers.json unreadable", func(w *Watch) {
			w.LoadServers = func() ([]vpnconfig.Server, error) { return nil, errors.New("no such file") }
		}},
		{"no TCP check", func(w *Watch) { w.Reachable = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := deadFake()
			w := reachWatch(f, osloServers(), map[string]bool{})
			tc.setup(w)
			assertDiesAt(t, w, f, DeadAfter)
		})
	}
}

// A working probe ends the run of misses, and the next run's streak starts
// from nothing: a look that found the server up in the last run does not keep
// the next one at three minutes.
func TestTick_HealthyProbeStartsANewUnreachableStreak(t *testing.T) {
	f := deadFake()
	up := map[string]bool{"203.0.113.10": true}
	w := reachWatch(f, osloServers(), up)
	w.Tick(context.Background()) // a miss while the server accepts TCP
	f.now = f.now.Add(ProbeInterval)
	f.probeErr = nil
	w.Tick(context.Background()) // the outbound works again
	f.now = f.now.Add(ProbeInterval)
	f.probeErr = errProbe
	delete(up, "203.0.113.10")
	assertDiesAt(t, w, f, FastDeadAfter)
}
