package subwatch

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/endpoint"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchcompat"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

// newKeeperFixture is the fast fixture with nobody on Xray: the watch is not
// armed, and the keeper looks after the server active_server names - Oslo, which
// the monitor holds dead, while Backup is alive.
func newKeeperFixture(t *testing.T) *fastFixture {
	t.Helper()
	s := newFastFixture(t)
	s.f.cfg.Xray.Clients = nil
	s.f.cfg.Xray.ActiveServer = vpnconfig.RecordActiveServer(&vpnconfig.ActiveServer{Seq: 6}, s.f.subs[0].Servers[0])
	s.setHealth()
	return s
}

func (s *fastFixture) setLatency(name string, ms int64) {
	for _, sub := range cloneSubs(s.f.subs) {
		for _, server := range sub.Servers {
			if server.Name != name {
				continue
			}
			for _, key := range endpoint.Keys(server) {
				for _, endpoints := range []map[string]watchdapi.EndpointState{s.h.cached.Endpoints, s.h.fresh.Endpoints} {
					st := endpoints[key]
					st.LatencyMS = ms
					endpoints[key] = st
				}
			}
		}
	}
}

func writtenNames(servers []vpnconfig.Server) []string {
	out := make([]string, len(servers))
	for i, s := range servers {
		out[i] = s.Name + "@" + s.Address
	}
	return out
}

// The provider moved Oslo to a new address: the keeper follows it at once.
func TestKeeper_FollowsAMovedServer(t *testing.T) {
	s := newKeeperFixture(t)
	s.f.subs[0].Servers[0] = fastServer("Oslo", "oslo-2.example", "203.0.113.30")
	s.w.Tick(context.Background())
	if got := writtenNames(s.written); !reflect.DeepEqual(got, []string{"Oslo@oslo-2.example"}) {
		t.Fatalf("written %v", got)
	}
	if a := s.f.cfg.Xray.ActiveServer; a == nil || a.Address != "oslo-2.example" || s.f.cfg.Xray.PreferredServer != nil {
		t.Fatalf("active %+v, preferred %+v", a, s.f.cfg.Xray.PreferredServer)
	}
	if want := []string{"Xray server Alpha / Oslo moved; Xray follows it"}; !reflect.DeepEqual(s.f.notes, want) {
		t.Fatalf("notes %v, want %v", s.f.notes, want)
	}
}

// New keys behind the same name and address: the keeper writes them.
func TestKeeper_FollowsAServerChangedInPlace(t *testing.T) {
	s := newKeeperFixture(t)
	oslo := s.f.subs[0].Servers[0]
	oslo.Outbound = json.RawMessage(strings.Replace(string(oslo.Outbound), "000000000010", "000000000011", 1))
	s.f.subs[0].Servers[0] = oslo
	s.w.Tick(context.Background())
	if a := s.f.cfg.Xray.ActiveServer; len(s.written) != 1 || a == nil || a.Identity != vpnconfig.IdentityMark(oslo) {
		t.Fatalf("written %v, active %+v", writtenNames(s.written), a)
	}
	if want := []string{"Xray server Alpha / Oslo changed; Xray follows it"}; !reflect.DeepEqual(s.f.notes, want) {
		t.Fatalf("notes %v, want %v", s.f.notes, want)
	}
}

// Oslo left its subscription: the best working server of its group replaces
// it - Backup, though the Shadowsocks server answers faster - and the vanished
// choice stays in preferred_server, its group with it.
func TestKeeper_ReplacesAServerThatLeftItsSubscription(t *testing.T) {
	s := newKeeperFixture(t)
	backup := s.f.subs[0].Servers[1]
	s.f.subs[0].Servers = []vpnconfig.Server{ssServer("Shadow", "198.51.100.40"), backup}
	s.setHealth()
	s.setLatency("Shadow", 5)
	s.w.Tick(context.Background())
	if got := writtenNames(s.written); !reflect.DeepEqual(got, []string{"Backup@backup.example"}) {
		t.Fatalf("written %v", got)
	}
	if p := s.f.cfg.Xray.PreferredServer; p == nil || p.Name != "Oslo" || p.Group != "vless+tls" {
		t.Fatalf("preferred %+v, want the vanished Oslo", p)
	}
	if want := []string{"Xray server Alpha / Oslo left its subscription; Xray switched to Alpha / Backup"}; !reflect.DeepEqual(s.f.notes, want) {
		t.Fatalf("notes %v, want %v", s.f.notes, want)
	}
}

// Without the monitor the keeper confirms the death the legacy way, then
// replaces the server.
func TestKeeper_ReplacesADeadServer(t *testing.T) {
	s := newKeeperFixture(t)
	s.w.Health = nil
	tickUntilDead(s.w, s.f)
	if got := writtenNames(s.written); !reflect.DeepEqual(got, []string{"Backup@backup.example"}) {
		t.Fatalf("written %v", got)
	}
	if p := s.f.cfg.Xray.PreferredServer; p == nil || p.Name != "Oslo" {
		t.Fatalf("preferred %+v, want Oslo kept for the return", p)
	}
	if want := []string{"Xray server Alpha / Oslo is not answering; Xray switched to Alpha / Backup"}; !reflect.DeepEqual(s.f.notes, want) {
		t.Fatalf("notes %v, want %v", s.f.notes, want)
	}
	if s.f.applies != 0 || s.f.cfg.Xray.Failover != nil {
		t.Fatalf("applies %d, failover %+v: nobody to move", s.f.applies, s.f.cfg.Xray.Failover)
	}
}

// Oslo's addresses refuse TCP while the WAN reaches a control address: the
// keeper replaces it after FastDeadAfter, not DeadAfter (spec 4.4).
func TestKeeper_ReplacesAnUnreachableServerAfterAMinute(t *testing.T) {
	s := newKeeperFixture(t)
	s.w.Health = nil
	s.w.Reachable = func(_ context.Context, ip string, _ int) bool { return !strings.HasPrefix(ip, "203.0.113.") }
	tickFor(s.w, s.f, FastDeadAfter-ProbeInterval)
	if len(s.written) != 0 {
		t.Fatalf("written %v before FastDeadAfter", writtenNames(s.written))
	}
	s.w.Tick(context.Background())
	if got := writtenNames(s.written); !reflect.DeepEqual(got, []string{"Backup@backup.example"}) {
		t.Fatalf("written %v at FastDeadAfter", got)
	}
	if want := []string{"Xray server Alpha / Oslo is not answering; Xray switched to Alpha / Backup"}; !reflect.DeepEqual(s.f.notes, want) {
		t.Fatalf("notes %v, want %v", s.f.notes, want)
	}
}

// With fresh evidence the fast path replaces the server within a tick, with
// its own message.
func TestKeeper_FastEvidenceSwitchesAtOnce(t *testing.T) {
	s := newKeeperFixture(t)
	s.passThenMiss(time.Second)
	s.w.Tick(context.Background())
	if got := writtenNames(s.written); !reflect.DeepEqual(got, []string{"Backup@backup.example"}) {
		t.Fatalf("written %v", got)
	}
	if want := []string{"Xray switched to server Alpha / Backup"}; !reflect.DeepEqual(s.f.notes, want) {
		t.Fatalf("notes %v, want %v", s.f.notes, want)
	}
}

// The entry Oslo moved to does not answer either: it is replaced as dead, and
// not tried again on the next tick.
func TestKeeper_AFollowedEntryThatDoesNotAnswerIsReplaced(t *testing.T) {
	s := newKeeperFixture(t)
	s.f.subs[0].Servers[0] = fastServer("Oslo", "oslo-2.example", "203.0.113.30")
	s.w.Probe = func(context.Context, int) error {
		s.probes++
		if len(s.written) > 0 && s.written[len(s.written)-1].Name == "Backup" {
			return nil
		}
		return errProbe
	}
	s.w.Tick(context.Background())
	if got := writtenNames(s.written); !reflect.DeepEqual(got, []string{"Oslo@oslo-2.example", "Backup@backup.example"}) {
		t.Fatalf("written %v", got)
	}
	if want := []string{"Xray server Alpha / Oslo is not answering; Xray switched to Alpha / Backup"}; !reflect.DeepEqual(s.f.notes, want) {
		t.Fatalf("notes %v, want %v", s.f.notes, want)
	}
	s.f.now = s.f.now.Add(ProbeInterval)
	s.w.Tick(context.Background())
	if len(s.written) != 2 {
		t.Fatalf("written %v; the next tick switched again", writtenNames(s.written))
	}
}

// Nothing works: one message, and the next attempt waits KeeperRetry instead
// of restarting Xray every 30 seconds.
func TestKeeper_NothingWorksSaysOnceAndBacksOff(t *testing.T) {
	s := newKeeperFixture(t)
	s.f.subs[0].Servers = s.f.subs[0].Servers[1:] // Oslo left; Backup alone
	s.setHealth()
	s.w.Probe = func(context.Context, int) error { s.probes++; return errProbe }
	s.w.Tick(context.Background())
	if want := []string{"No working Xray server found; Xray stays on Alpha / Backup"}; !reflect.DeepEqual(s.f.notes, want) {
		t.Fatalf("notes %v, want %v", s.f.notes, want)
	}
	tried := s.generateCalls
	tickFor(s.w, s.f, KeeperRetry-time.Minute)
	if s.generateCalls != tried {
		t.Fatalf("generate %d within the backoff, want %d", s.generateCalls, tried)
	}
	tickFor(s.w, s.f, 2*time.Minute)
	if len(s.f.notes) != 1 {
		t.Fatalf("notes %v; the run of failures is said once", s.f.notes)
	}
}

// A /stop while the keeper switches ends it: nothing restarts, nothing is said.
func TestKeeper_AStopEndsTheSwitchSilently(t *testing.T) {
	s := newKeeperFixture(t)
	s.f.subs[0].Servers[0] = fastServer("Oslo", "oslo-2.example", "203.0.113.30")
	stopped := false
	s.w.Stopped = func() bool { return stopped }
	generate := s.w.Generate
	s.w.Generate = func(server vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		stopped = true
		return generate(server, guard)
	}
	s.w.Tick(context.Background())
	if s.restarts != 0 || len(s.f.notes) != 0 || len(s.written) != 0 {
		t.Fatalf("restarts %d, notes %v, written %v after a stop", s.restarts, s.f.notes, writtenNames(s.written))
	}
}

// The compatibility gate closing while the keeper switches ends it as a stop does.
func TestKeeper_AClosedGateEndsTheSwitchSilently(t *testing.T) {
	s := newKeeperFixture(t)
	s.f.subs[0].Servers[0] = fastServer("Oslo", "oslo-2.example", "203.0.113.30")
	var closed error
	s.w.CanMutate = func() error { return closed }
	generate := s.w.Generate
	s.w.Generate = func(server vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		closed = watchcompat.ErrIncompatible
		return generate(server, guard)
	}
	s.w.Tick(context.Background())
	if s.restarts != 0 || len(s.f.notes) != 0 || len(s.written) != 0 {
		t.Fatalf("restarts %d, notes %v, written %v after the gate closed", s.restarts, s.f.notes, writtenNames(s.written))
	}
}

// A selection the user commits while the keeper switches stands.
func TestKeeper_ASelectionDuringTheSwitchStands(t *testing.T) {
	s := newKeeperFixture(t)
	s.f.subs[0].Servers[0] = fastServer("Oslo", "oslo-2.example", "203.0.113.30")
	backup := s.f.subs[0].Servers[1]
	generate := s.w.Generate
	s.w.Generate = func(server vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		s.f.cfg.Xray.ActiveServer = vpnconfig.RecordActiveServer(s.f.cfg.Xray.ActiveServer, backup)
		return generate(server, guard)
	}
	s.w.Tick(context.Background())
	if a := s.f.cfg.Xray.ActiveServer; a == nil || a.Name != "Backup" || len(s.written) != 0 || len(s.f.notes) != 0 {
		t.Fatalf("active %+v, written %v, notes %v; the user's selection must stand", a, writtenNames(s.written), s.f.notes)
	}
}

// A record without a subscription is nothing the keeper can look after.
func TestKeeper_ARecordWithoutASubscriptionIsLeftAlone(t *testing.T) {
	s := newKeeperFixture(t)
	s.f.cfg.Xray.ActiveServer.Subscription = ""
	s.w.Health = nil
	tickUntilDead(s.w, s.f)
	if s.generateCalls != 0 || len(s.f.notes) != 0 {
		t.Fatalf("generate %d, notes %v", s.generateCalls, s.f.notes)
	}
}

// Without xray installed the keeper does nothing.
func TestKeeper_NoXrayDoesNothing(t *testing.T) {
	s := newKeeperFixture(t)
	s.f.subs[0].Servers[0] = fastServer("Oslo", "oslo-2.example", "203.0.113.30")
	s.h.cached.State = watchdapi.StateNoXray
	s.w.Tick(context.Background())
	if s.generateCalls != 0 {
		t.Fatalf("generate %d with no xray", s.generateCalls)
	}
}

// With clients on Xray the same actuality check runs before the probe.
func TestTick_WithClientsFollowsAMovedServer(t *testing.T) {
	s := newFastFixture(t)
	s.f.cfg.Xray.ActiveServer = vpnconfig.RecordActiveServer(&vpnconfig.ActiveServer{Seq: 6}, s.f.subs[0].Servers[0])
	s.f.subs[0].Servers[0] = fastServer("Oslo", "oslo-2.example", "203.0.113.30")
	s.w.Tick(context.Background())
	if got := writtenNames(s.written); !reflect.DeepEqual(got, []string{"Oslo@oslo-2.example"}) || s.f.cfg.Xray.Failover != nil {
		t.Fatalf("written %v, failover %+v", got, s.f.cfg.Xray.Failover)
	}
	if want := []string{fmt.Sprintf(msgFollowedMoved, "Alpha / Oslo")}; !reflect.DeepEqual(s.f.notes, want) {
		t.Fatalf("notes %v, want %v", s.f.notes, want)
	}
}

// An apply of the watch's own still pending is an episode, and the episode
// owns the record (spec 4.2): the moved server waits for it to end.
func TestTick_DuringAnEpisodeAMovedServerIsNotFollowed(t *testing.T) {
	s := newFastFixture(t)
	s.f.cfg.Xray.ActiveServer = vpnconfig.RecordActiveServer(&vpnconfig.ActiveServer{Seq: 6}, s.f.subs[0].Servers[0])
	s.f.subs[0].Servers[0] = fastServer("Oslo", "oslo-2.example", "203.0.113.30")
	s.w.pendingApply = true
	s.f.applyErr = errApply
	s.w.Tick(context.Background())
	if a := s.f.cfg.Xray.ActiveServer; s.f.applies != 1 || len(s.written) != 0 || len(s.f.notes) != 0 || a.Address != "oslo.example" {
		t.Fatalf("applies %d, written %v, notes %v, active %+v; the pending apply's episode owns the record",
			s.f.applies, writtenNames(s.written), s.f.notes, a)
	}
}
