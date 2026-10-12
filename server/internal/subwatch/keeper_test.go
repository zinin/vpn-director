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
	"github.com/zinin/vpn-director/server/internal/monitor"
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

// A selection the user commits while the probe of the followed entry waits
// ends the action: no replacement writes or restarts anything, nothing is
// said, and the user's server stands.
func TestKeeper_ASelectionDuringTheFollowsProbeStands(t *testing.T) {
	s := newKeeperFixture(t)
	third := fastServer("Third", "third.example", "198.51.100.30")
	s.f.subs[0].Servers = append(s.f.subs[0].Servers, third)
	s.setHealth()
	s.f.subs[0].Servers[0] = fastServer("Oslo", "oslo-2.example", "203.0.113.30")
	s.w.Probe = func(context.Context, int) error {
		s.probes++
		if s.probes == 1 {
			s.f.cfg.Xray.ActiveServer = vpnconfig.RecordActiveServer(s.f.cfg.Xray.ActiveServer, third)
		}
		return errProbe
	}
	s.w.Tick(context.Background())
	if a := s.f.cfg.Xray.ActiveServer; a.Name != "Third" || !reflect.DeepEqual(writtenNames(s.written), []string{"Oslo@oslo-2.example"}) ||
		s.restarts != 1 || len(s.f.notes) != 0 {
		t.Fatalf("active %+v, written %v, restarts %d, notes %v; the user's selection must end the action",
			a, writtenNames(s.written), s.restarts, s.f.notes)
	}
}

// A selection the user commits while the probe of the tick that confirms the
// death waits ends the replacement the same way: the death was confirmed on
// the record that tick read, not on the user's server.
func TestKeeper_ASelectionDuringTheDeathTicksProbeStands(t *testing.T) {
	s := newKeeperFixture(t)
	third := fastServer("Third", "third.example", "198.51.100.30")
	s.f.subs[0].Servers = append(s.f.subs[0].Servers, third)
	s.w.Health = nil
	died := s.f.now
	s.w.Probe = func(context.Context, int) error {
		s.probes++
		if s.f.now.Sub(died) == DeadAfter && s.f.cfg.Xray.ActiveServer.Name == "Oslo" {
			s.f.cfg.Xray.ActiveServer = vpnconfig.RecordActiveServer(s.f.cfg.Xray.ActiveServer, third)
		}
		return errProbe
	}
	tickUntilDead(s.w, s.f)
	if a := s.f.cfg.Xray.ActiveServer; a.Name != "Third" || len(s.written) != 0 || s.restarts != 0 || len(s.f.notes) != 0 {
		t.Fatalf("active %+v, written %v, restarts %d, notes %v; the user's selection must end the replacement",
			a, writtenNames(s.written), s.restarts, s.f.notes)
	}
}

// A selection committed during the last probe of a run that finds nothing
// working ends the action too: the run did not fail on the user's server, so
// it says nothing and backs nothing off.
func TestKeeper_ASelectionDuringTheLastProbeEndsTheRun(t *testing.T) {
	s := newKeeperFixture(t)
	backup := s.f.subs[0].Servers[1]
	s.f.subs[0].Servers = []vpnconfig.Server{backup, fastServer("Third", "third.example", "198.51.100.30")} // Oslo left
	s.setHealth()
	s.w.Probe = func(context.Context, int) error {
		s.probes++
		if s.written[len(s.written)-1].Name == "Third" {
			s.f.cfg.Xray.ActiveServer = vpnconfig.RecordActiveServer(s.f.cfg.Xray.ActiveServer, backup)
		}
		return errProbe
	}
	s.w.Tick(context.Background())
	if a := s.f.cfg.Xray.ActiveServer; a.Name != "Backup" || len(s.f.notes) != 0 || !s.w.keeperNotBefore.IsZero() {
		t.Fatalf("active %+v, notes %v, next attempt %v; the selection must end the run", a, s.f.notes, s.w.keeperNotBefore)
	}
}

// The fast path proved Oslo dead and found no candidate alive in its fresh
// checks: the keeper replaces Oslo under the fast path's ownership (Ruling
// T5-R2), which checks and records the keeper's write, so the switch goes on
// to its restart and probe.
func TestKeeper_TheFastPathsFallbackIsReplacedUnderItsOwnership(t *testing.T) {
	s := newKeeperFixture(t)
	s.passThenMiss(time.Second)
	key := s.candidateKey()
	backup := s.h.fresh.Endpoints[key]
	backup.Status = watchdapi.StatusDead
	s.h.fresh.Endpoints[key] = backup
	s.w.Tick(context.Background())
	if got := writtenNames(s.written); !reflect.DeepEqual(got, []string{"Backup@backup.example"}) || s.restarts != 1 {
		t.Fatalf("written %v, restarts %d", got, s.restarts)
	}
	if want := []string{"Xray server Alpha / Oslo is not answering; Xray switched to Alpha / Backup"}; !reflect.DeepEqual(s.f.notes, want) {
		t.Fatalf("notes %v, want %v", s.f.notes, want)
	}
}

// A working probe ends a run of failures (Ruling T5-R1): the next death is
// replaced as soon as the death rules confirm it, not after the old run's
// backoff, and a new run that finds nothing working backs off KeeperRetry
// again, not twice that. Each run says "No working Xray server found" once.
func TestKeeper_AWorkingProbeEndsTheBackoff(t *testing.T) {
	s := newKeeperFixture(t)
	s.w.Health = nil
	backup := s.f.subs[0].Servers[1]
	s.f.subs[0].Servers = []vpnconfig.Server{backup, fastServer("Spare", "spare.example", "198.51.100.30")} // Oslo left
	working := false
	s.w.Probe = func(context.Context, int) error {
		s.probes++
		if working {
			return nil
		}
		return errProbe
	}
	s.w.Tick(context.Background()) // Backup and Spare do not answer: the run backs off
	s.f.now = s.f.now.Add(ProbeInterval)
	working = true
	s.w.Tick(context.Background()) // Xray works on Spare again
	s.f.now = s.f.now.Add(ProbeInterval)
	working = false
	tickUntilDead(s.w, s.f) // a new death, well inside the first run's backoff
	if got := writtenNames(s.written); !reflect.DeepEqual(got, []string{"Backup@backup.example", "Spare@spare.example", "Backup@backup.example"}) {
		t.Fatalf("written %v; the first run's backoff must not hold the new death", got)
	}
	s.f.now = s.f.now.Add(ProbeInterval)
	tickFor(s.w, s.f, KeeperRetry-2*ProbeInterval)
	if len(s.written) != 3 {
		t.Fatalf("written %v within KeeperRetry of the second run", writtenNames(s.written))
	}
	s.w.Tick(context.Background())
	if len(s.written) != 4 {
		t.Fatalf("written %v at KeeperRetry after the second run; a new run backs off KeeperRetry again", writtenNames(s.written))
	}
	want := []string{"No working Xray server found; Xray stays on Alpha / Spare", "No working Xray server found; Xray stays on Alpha / Backup"}
	if !reflect.DeepEqual(s.f.notes, want) {
		t.Fatalf("notes %v, want %v", s.f.notes, want)
	}
}

// With clients on Xray a working probe ends the keeper's run as well (Ruling
// T5-R1): a server that moves afterwards is followed at once, not after the
// backoff of a run that found nothing working.
func TestTick_AWorkingProbeEndsTheKeeperBackoff(t *testing.T) {
	s := newFastFixture(t)
	s.f.cfg.Xray.ActiveServer = vpnconfig.RecordActiveServer(&vpnconfig.ActiveServer{Seq: 6}, s.f.subs[0].Servers[0])
	s.f.subs[0].Servers = s.f.subs[0].Servers[1:] // Oslo left; Backup alone
	working := false
	s.w.Probe = func(context.Context, int) error {
		s.probes++
		if working {
			return nil
		}
		return errProbe
	}
	s.w.Tick(context.Background()) // Oslo replaced by Backup, which does not answer: the run backs off
	s.f.now = s.f.now.Add(ProbeInterval)
	working = true
	s.w.Tick(context.Background()) // Xray works on Backup
	s.f.now = s.f.now.Add(ProbeInterval)
	s.f.subs[0].Servers[0] = fastServer("Backup", "backup-2.example", "198.51.100.21")
	s.w.Tick(context.Background())
	if got := writtenNames(s.written); !reflect.DeepEqual(got, []string{"Backup@backup.example", "Backup@backup-2.example"}) {
		t.Fatalf("written %v; the moved server must be followed at once", got)
	}
	want := []string{"No working Xray server found; Xray stays on Alpha / Backup", "Xray server Alpha / Backup moved; Xray follows it"}
	if !reflect.DeepEqual(s.f.notes, want) {
		t.Fatalf("notes %v, want %v", s.f.notes, want)
	}
}

// The replacement consumes the fast path's death proof (Ruling T5-R4): checks
// the monitor completes while the keeper switches make the proof stale, and
// the switch still goes through, proved by its own probe.
func TestKeeper_AStaleDeathProofDoesNotStopTheReplacement(t *testing.T) {
	s := newKeeperFixture(t)
	s.passThenMiss(time.Second)
	key := s.candidateKey()
	backup := s.h.fresh.Endpoints[key]
	backup.Status = watchdapi.StatusDead
	s.h.fresh.Endpoints[key] = backup
	generate := s.w.Generate
	s.w.Generate = func(server vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		generated, seq, err := generate(server, guard)
		for _, proved := range append(s.activeKeys(), key) {
			s.h.invalidate(proved)
		}
		return generated, seq, err
	}
	s.w.Tick(context.Background())
	if got := writtenNames(s.written); !reflect.DeepEqual(got, []string{"Backup@backup.example"}) || s.restarts != 1 {
		t.Fatalf("written %v, restarts %d; a stale proof must not stop the replacement", got, s.restarts)
	}
	if want := []string{"Xray server Alpha / Oslo is not answering; Xray switched to Alpha / Backup"}; !reflect.DeepEqual(s.f.notes, want) {
		t.Fatalf("notes %v, want %v", s.f.notes, want)
	}
}

// The fast path's ownership itself refuses during the replacement - the inbound
// ports change - and the action ends there (Ruling T5-R4): nothing more is
// written or restarted, nothing is said, nothing backs off, and no refused
// candidate logs a warning. The fast path tried Backup, and the keeper tries
// Oslo's two addresses; the refusal comes at the restart after a write, at the
// next write after a probe, or after the last probe.
func TestKeeper_ARefusedOwnershipEndsTheReplacement(t *testing.T) {
	for _, tc := range []struct {
		name     string
		written  bool   // the ports change right after the keeper's first write,
		probed   string // or while the probe of the copy on this address waits
		names    []string
		restarts int
	}{
		{"at the restart", true, "", []string{"Backup@backup.example", "Oslo@oslo.example"}, 1},
		{"at the next write", false, "203.0.113.10", []string{"Backup@backup.example", "Oslo@oslo.example"}, 2},
		{"after the last probe", false, "203.0.113.11", []string{"Backup@backup.example", "Oslo@oslo.example", "Oslo@oslo.example"}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newKeeperFixture(t)
			s.passThenMiss(time.Second)
			log := captureLog(t)
			changePorts := func() {
				s.f.cfg.Advanced = map[string]interface{}{"xray": map[string]interface{}{"socks_port": float64(22346)}}
			}
			s.w.Probe = func(context.Context, int) error { // nothing answers, Backup in the fast path neither
				s.probes++
				if len(s.written) > 1 && s.written[len(s.written)-1].IPs[0] == tc.probed {
					changePorts()
				}
				return errProbe
			}
			generate := s.w.Generate
			s.w.Generate = func(server vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
				generated, seq, err := generate(server, guard)
				if tc.written && s.generateCalls == 2 {
					changePorts()
				}
				return generated, seq, err
			}
			s.w.Tick(context.Background())
			if got := writtenNames(s.written); !reflect.DeepEqual(got, tc.names) || s.restarts != tc.restarts {
				t.Fatalf("written %v, restarts %d; want %v and %d", got, s.restarts, tc.names, tc.restarts)
			}
			if len(s.f.notes) != 0 || !s.w.keeperNotBefore.IsZero() || strings.Contains(log.String(), "level=WARN") {
				t.Fatalf("notes %v, next attempt %v, log:\n%s\na refused ownership says, backs off and warns of nothing", s.f.notes, s.w.keeperNotBefore, log)
			}
		})
	}
}

// setStates replaces the monitor's view: every endpoint of each named server
// takes the state given.
func (s *fastFixture) setStates(states map[string]watchdapi.EndpointState) {
	e := monitor.Evidence{State: watchdapi.StateOK, Endpoints: map[string]watchdapi.EndpointState{}}
	for _, sub := range cloneSubs(s.f.subs) {
		for _, server := range sub.Servers {
			if st, ok := states[server.Name]; ok {
				for _, key := range endpoint.Keys(server) {
					e.Endpoints[key] = st
				}
			}
		}
	}
	s.h = &fastHealth{cached: e, fresh: copyFastEvidence(e, nil), invalid: map[string]bool{}}
	s.w.Health = s.h
}

func aliveSince(since time.Time, ms int64) watchdapi.EndpointState {
	return watchdapi.EndpointState{Status: watchdapi.StatusAlive, LatencyMS: ms, Since: since, CheckedAt: since}
}

// newReturnKeeper is the keeper fixture running Backup after Oslo, the user's
// choice, died: Xray works, and the return looks every ReturnCheck.
func newReturnKeeper(t *testing.T) *fastFixture {
	t.Helper()
	s := newKeeperFixture(t)
	oslo, backup := s.f.subs[0].Servers[0], s.f.subs[0].Servers[1]
	s.f.cfg.Xray.PreferredServer = vpnconfig.NewActiveServer(oslo)
	s.f.cfg.Xray.ActiveServer = vpnconfig.RecordActiveServer(s.f.cfg.Xray.ActiveServer, backup)
	s.w.Probe = func(context.Context, int) error { s.probes++; return nil }
	return s
}

// The monitor has held Oslo alive for six minutes: Xray goes back to it.
func TestMaybeReturn_AfterFiveMinutesAliveReturns(t *testing.T) {
	s := newReturnKeeper(t)
	s.setStates(map[string]watchdapi.EndpointState{
		"Oslo": aliveSince(s.f.now.Add(-6*time.Minute), 40), "Backup": aliveSince(s.f.now.Add(-time.Hour), 17),
	})
	s.w.Tick(context.Background())
	if got := writtenNames(s.written); !reflect.DeepEqual(got, []string{"Oslo@oslo.example"}) || s.f.cfg.Xray.PreferredServer != nil {
		t.Fatalf("written %v, preferred %+v", got, s.f.cfg.Xray.PreferredServer)
	}
	if want := []string{"Xray back on the preferred server Alpha / Oslo"}; !reflect.DeepEqual(s.f.notes, want) {
		t.Fatalf("notes %v, want %v", s.f.notes, want)
	}
}

// Alive for two minutes is not yet a server to go back to.
func TestMaybeReturn_TwoMinutesAliveIsNotEnough(t *testing.T) {
	s := newReturnKeeper(t)
	s.setStates(map[string]watchdapi.EndpointState{
		"Oslo": aliveSince(s.f.now.Add(-2*time.Minute), 40), "Backup": aliveSince(s.f.now.Add(-time.Hour), 17),
	})
	s.w.Tick(context.Background())
	if len(s.written) != 0 {
		t.Fatalf("written %v", writtenNames(s.written))
	}
}

// groupReturnStates is the monitor's view of newGroupReturnKeeper at now - Oslo
// dead for an hour, Shadow alive for an hour, Backup alive for six minutes -
// with the states in more added or put in their place.
func groupReturnStates(now time.Time, more map[string]watchdapi.EndpointState) map[string]watchdapi.EndpointState {
	states := map[string]watchdapi.EndpointState{
		"Oslo":   {Status: watchdapi.StatusDead, Since: now.Add(-time.Hour)},
		"Shadow": aliveSince(now.Add(-time.Hour), 5),
		"Backup": aliveSince(now.Add(-6*time.Minute), 17),
	}
	for name, st := range more {
		states[name] = st
	}
	return states
}

// newGroupReturnKeeper is the return keeper with Xray on Shadow, a Shadowsocks
// server, while Oslo, the user's vless+tls choice, is dead and Backup, of
// Oslo's group, has been alive six minutes.
func newGroupReturnKeeper(t *testing.T) *fastFixture {
	t.Helper()
	s := newReturnKeeper(t)
	oslo, backup := s.f.subs[0].Servers[0], s.f.subs[0].Servers[1]
	shadow := ssServer("Shadow", "198.51.100.40")
	s.f.subs[0].Servers = []vpnconfig.Server{oslo, shadow, backup}
	s.f.cfg.Xray.ActiveServer = vpnconfig.RecordActiveServer(s.f.cfg.Xray.ActiveServer, shadow)
	s.setStates(groupReturnStates(s.f.now, nil))
	return s
}

// Xray runs a Shadowsocks server while Oslo, the user's vless+tls choice, is
// dead: once Backup, of Oslo's group, has been alive five minutes, Xray goes
// there, and Oslo stays the choice to return to.
func TestMaybeReturn_BackToTheUsersGroup(t *testing.T) {
	s := newGroupReturnKeeper(t)
	s.w.Tick(context.Background())
	if got := writtenNames(s.written); !reflect.DeepEqual(got, []string{"Backup@backup.example"}) {
		t.Fatalf("written %v", got)
	}
	if p := s.f.cfg.Xray.PreferredServer; p == nil || p.Name != "Oslo" {
		t.Fatalf("preferred %+v, want Oslo kept", p)
	}
	if want := []string{"Xray back on a vless+tls server Alpha / Backup"}; !reflect.DeepEqual(s.f.notes, want) {
		t.Fatalf("notes %v, want %v", s.f.notes, want)
	}
}

// aliveFor keeps a copy the monitor has held alive for d or longer - exactly d
// included - fastest first, ties in the order given; a copy alive for less, a
// dead one and one the monitor does not know are left out.
func TestAliveFor(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	a := fastServer("A", "a.example", "198.51.100.1")
	b := fastServer("B", "b.example", "198.51.100.2")
	c := fastServer("C", "c.example", "198.51.100.3")
	young := fastServer("Young", "young.example", "198.51.100.4")
	dead := fastServer("Dead", "dead.example", "198.51.100.5")
	unknown := fastServer("Unknown", "unknown.example", "198.51.100.6")
	ev := monitor.Evidence{State: watchdapi.StateOK, Endpoints: map[string]watchdapi.EndpointState{
		endpoint.Key(a):     aliveSince(now.Add(-ReturnAlive), 30),
		endpoint.Key(b):     aliveSince(now.Add(-time.Hour), 10),
		endpoint.Key(c):     aliveSince(now.Add(-time.Hour), 30),
		endpoint.Key(young): aliveSince(now.Add(-ReturnAlive+time.Second), 1),
		endpoint.Key(dead):  {Status: watchdapi.StatusDead, Since: now.Add(-time.Hour)},
	}}
	got := aliveFor([]vpnconfig.Server{a, young, b, dead, c, unknown}, ev, now, ReturnAlive)
	if names := bestNames(got); !reflect.DeepEqual(names, []string{"B", "A", "C"}) {
		t.Fatalf("aliveFor %v, want [B A C]", names)
	}
}

// Back to the group goes by the order of a replacement (spec 4.6): a server of
// the group in the user's subscription before a faster one elsewhere.
func TestMaybeReturn_BackToTheGroupPrefersTheUsersSubscription(t *testing.T) {
	s := newGroupReturnKeeper(t)
	s.f.subs = append(s.f.subs, vpnconfig.Subscription{ID: "beta", Name: "Beta", URL: "https://beta.example/list",
		Servers: []vpnconfig.Server{fastServer("Faster", "faster.example", "198.51.100.30")}})
	s.setStates(groupReturnStates(s.f.now, map[string]watchdapi.EndpointState{"Faster": aliveSince(s.f.now.Add(-6*time.Minute), 3)}))
	s.w.Tick(context.Background())
	if got := writtenNames(s.written); !reflect.DeepEqual(got, []string{"Backup@backup.example"}) {
		t.Fatalf("written %v, want Backup of the user's subscription before Beta / Faster", got)
	}
	if want := []string{"Xray back on a vless+tls server Alpha / Backup"}; !reflect.DeepEqual(s.f.notes, want) {
		t.Fatalf("notes %v, want %v", s.f.notes, want)
	}
}

// Back to the group tries the best server of the group alone (Ruling T6-R1,
// spec 4.6): Backup's probe fails, so Xray goes back to Shadow and the next
// attempt backs off - Spare, alive too, is not tried. The log names the group
// the return was for, not the preferred server (Ruling T6-R3).
func TestMaybeReturn_BackToTheGroupTriesItsBestServerAlone(t *testing.T) {
	s := newGroupReturnKeeper(t)
	s.f.subs[0].Servers = append(s.f.subs[0].Servers, fastServer("Spare", "spare.example", "198.51.100.30"))
	s.setStates(groupReturnStates(s.f.now, map[string]watchdapi.EndpointState{"Spare": aliveSince(s.f.now.Add(-6*time.Minute), 20)}))
	s.w.Probe = func(context.Context, int) error {
		s.probes++
		if len(s.written) > 0 && s.written[len(s.written)-1].Name == "Backup" {
			return errProbe
		}
		return nil
	}
	log := captureLog(t)
	s.w.Tick(context.Background())
	if got := writtenNames(s.written); !reflect.DeepEqual(got, []string{"Backup@backup.example", "Shadow@198.51.100.40"}) {
		t.Fatalf("written %v; want Backup alone, then back on Shadow", got)
	}
	if a := s.f.cfg.Xray.ActiveServer; a.Name != "Shadow" || s.w.returnFails != 1 || len(s.f.notes) != 0 {
		t.Fatalf("active %+v, returnFails %d, notes %v; a failed return backs off and tells nobody", a, s.w.returnFails, s.f.notes)
	}
	if out := log.String(); !strings.Contains(out, "Return to a server of the preferred group failed; back on the previous server") ||
		strings.Contains(out, "preferred server") {
		t.Fatalf("log:\n%s\nthe lines of a return to the group name the group, not the preferred server", out)
	}
}

// A preferred record an older writer left without its group takes the group
// of its entry (Ruling T6-R2, spec 4.5): Xray goes back to that group all the
// same.
func TestMaybeReturn_BackToTheGroupOfAPreferredRecordWithoutOne(t *testing.T) {
	s := newGroupReturnKeeper(t)
	p := s.f.cfg.Xray.PreferredServer
	p.Identity, p.Group = "", ""
	s.w.Tick(context.Background())
	if got := writtenNames(s.written); !reflect.DeepEqual(got, []string{"Backup@backup.example"}) {
		t.Fatalf("written %v", got)
	}
	if want := []string{"Xray back on a vless+tls server Alpha / Backup"}; !reflect.DeepEqual(s.f.notes, want) {
		t.Fatalf("notes %v, want %v", s.f.notes, want)
	}
}

// A return to the user's group that has held for ReturnHold ends the backoff,
// as a held return to the preferred server does (Ruling T6-R4). Three returns
// to Oslo had failed before it; the failed returns to Oslo after it start a
// new count, and the returns go on instead of stopping at ReturnFailsMax.
func TestMaybeReturn_AHeldGroupReturnEndsTheBackoff(t *testing.T) {
	s := newGroupReturnKeeper(t)
	s.w.returnFails, s.w.returnRetry = ReturnFailsMax-1, ReturnRetryMax
	s.w.Tick(context.Background()) // back on Backup, of Oslo's group
	if got := writtenNames(s.written); !reflect.DeepEqual(got, []string{"Backup@backup.example"}) {
		t.Fatalf("written %v", got)
	}
	// Backup has held. The monitor has Oslo back, but its probe fails.
	s.f.now = s.f.now.Add(ReturnHold)
	s.setStates(groupReturnStates(s.f.now, map[string]watchdapi.EndpointState{"Oslo": aliveSince(s.f.now.Add(-ReturnAlive), 40)}))
	s.w.Probe = func(context.Context, int) error {
		s.probes++
		if s.written[len(s.written)-1].Name == "Oslo" {
			return errProbe
		}
		return nil
	}
	s.w.Tick(context.Background())
	s.f.now = s.f.now.Add(ReturnRetry)
	s.w.Tick(context.Background())
	tries := 0
	for _, server := range s.written {
		if server.Name == "Oslo" {
			tries++
		}
	}
	if tries != 4 || s.w.returnFails != 2 {
		t.Fatalf("written %v, returnFails %d; want two returns to Oslo's two addresses, counted anew",
			writtenNames(s.written), s.w.returnFails)
	}
}
