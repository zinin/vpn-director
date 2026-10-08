package subwatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/endpoint"
	"github.com/zinin/vpn-director/server/internal/monitor"
	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchcompat"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

type fastCheckCall struct {
	keys     []string
	started  time.Time
	deadline time.Time
	bounded  bool
}

type fastHealth struct {
	mu       sync.Mutex
	cached   monitor.Evidence
	fresh    monitor.Evidence
	calls    []fastCheckCall
	invalid  map[string]bool
	check    func(context.Context, []string) (monitor.Evidence, error)
	validate func(monitor.Evidence, []string) error
}

var _ HealthMonitor = (*fastHealth)(nil)

func copyFastEvidence(e monitor.Evidence, keys []string) monitor.Evidence {
	out := e
	out.Endpoints = make(map[string]watchdapi.EndpointState)
	if keys == nil {
		for key, state := range e.Endpoints {
			out.Endpoints[key] = state
		}
	} else {
		for _, key := range keys {
			if state, ok := e.Endpoints[key]; ok {
				out.Endpoints[key] = state
			}
		}
	}
	return out
}

func (h *fastHealth) Evidence() monitor.Evidence {
	h.mu.Lock()
	defer h.mu.Unlock()
	return copyFastEvidence(h.cached, nil)
}

func (h *fastHealth) CheckEvidence(ctx context.Context, keys []string) (monitor.Evidence, error) {
	deadline, bounded := ctx.Deadline()
	h.mu.Lock()
	h.calls = append(h.calls, fastCheckCall{append([]string(nil), keys...), time.Now(), deadline, bounded})
	check := h.check
	out := copyFastEvidence(h.fresh, keys)
	h.mu.Unlock()
	if check != nil {
		return check(ctx, keys)
	}
	return out, nil
}

func (h *fastHealth) ValidateEvidence(e monitor.Evidence, keys []string) error {
	h.mu.Lock()
	validate := h.validate
	invalid := false
	for _, key := range keys {
		if _, ok := e.Endpoints[key]; !ok || h.invalid[key] {
			invalid = true
		}
	}
	h.mu.Unlock()
	if invalid || e.State != watchdapi.StateOK || len(keys) == 0 {
		return errors.New("synthetic stale evidence")
	}
	if validate != nil {
		return validate(e, keys)
	}
	return nil
}

func (h *fastHealth) requests() []fastCheckCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]fastCheckCall(nil), h.calls...)
}

func (h *fastHealth) invalidate(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.invalid[key] = true
}

func fastServer(name, host, ip string) vpnconfig.Server {
	return vpnconfig.Server{
		Subscription: "alpha", Name: name, Address: host, Port: 443, IPs: []string{ip},
		Outbound: json.RawMessage(fmt.Sprintf(`{"protocol":"vless","settings":{"vnext":[{"address":%q,"port":443,"users":[{"id":"00000000-0000-4000-8000-000000000010","encryption":"none"}]}]},"streamSettings":{"network":"tcp","security":"tls","tlsSettings":{"serverName":%q}}}`, host, host)),
	}
}

type fastFixture struct {
	f             *fake
	w             *Watch
	h             *fastHealth
	generateCalls int
	written       []vpnconfig.Server
	restarts      int
	probes        int
	updates       int
	saves         int
	wanCalls      atomic.Int32
	events        []string
}

func newFastFixture(t *testing.T) *fastFixture {
	t.Helper()
	active := fastServer("Oslo", "oslo.example", "203.0.113.10")
	active.IPs = []string{"203.0.113.10", "203.0.113.11"}
	backup := fastServer("Backup", "backup.example", "198.51.100.20")
	// Tunnel Director requires the existing RFC1918 LAN fixture.
	cfg := baseCfg()
	cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Subscription: "alpha", Name: "Oslo", Address: "oslo.example", Port: 443, Seq: 7}
	cfg.Xray.Servers = []string{"203.0.113.10", "203.0.113.11", "198.51.100.20"}
	f := &fake{
		cfg: cfg, plat: connected("ovpnc2"), now: time.Unix(1_700_000_000, 0),
		subs: []vpnconfig.Subscription{{ID: "alpha", Name: "Alpha", URL: "https://alpha.example/list", Servers: []vpnconfig.Server{active, backup}}},
	}
	s := &fastFixture{f: f, w: f.watch()}
	s.setHealth()
	s.w.WANUp = func(context.Context) bool { s.wanCalls.Add(1); return true }
	update, save := s.w.UpdateVPN, s.w.SaveSubscription
	s.w.UpdateVPN = func(fn func(*vpnconfig.VPNDirectorConfig) error) error { s.updates++; return update(fn) }
	s.w.SaveSubscription = func(sub vpnconfig.Subscription) error { s.saves++; return save(sub) }
	s.w.Generate = func(server vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		s.generateCalls++
		if err := f.checkGuard(guard); err != nil {
			return false, f.seq(), err
		}
		s.written = append(s.written, server)
		s.events = append(s.events, "generate "+server.Name)
		vpnconfig.RecordWalkedServer(f.cfg, server)
		return true, f.seq(), nil
	}
	s.w.RestartXray = func() error {
		s.restarts++
		s.events = append(s.events, "restart")
		return nil
	}
	s.w.AfterRestart = func(delay time.Duration) {
		if delay != 3*time.Second {
			t.Errorf("settle %v, want 3 seconds before the main HTTPS probe", delay)
		}
		s.events = append(s.events, "settle")
	}
	s.w.Probe = func(ctx context.Context, port int) error {
		s.probes++
		s.events = append(s.events, "probe")
		if ctx.Err() != nil || port != 12346 {
			t.Errorf("main probe context %v, SOCKS port %d", ctx.Err(), port)
		}
		if len(s.written) == 0 {
			return errProbe
		}
		return nil
	}
	return s
}

func (s *fastFixture) setHealth() {
	cached := monitor.Evidence{State: watchdapi.StateOK, Endpoints: make(map[string]watchdapi.EndpointState)}
	fresh := monitor.Evidence{State: watchdapi.StateOK, Endpoints: make(map[string]watchdapi.EndpointState)}
	active := s.f.cfg.Xray.ActiveServer
	for _, sub := range cloneSubs(s.f.subs) {
		for _, server := range sub.Servers {
			status := watchdapi.StatusAlive
			if active != nil && server.Name == active.Name && sub.ID == active.Subscription {
				status = watchdapi.StatusDead
			}
			for _, key := range endpoint.Keys(server) {
				state := watchdapi.EndpointState{Status: status, CheckedAt: s.f.now, NextAt: s.f.now.Add(time.Minute), Since: s.f.now}
				if status == watchdapi.StatusAlive {
					state.LatencyMS = 17
				} else {
					state.Fails, state.Error = 1, "timeout"
				}
				fresh.Endpoints[key] = state
				state.CheckedAt = s.f.now.Add(-time.Minute)
				cached.Endpoints[key] = state
			}
		}
	}
	s.h = &fastHealth{cached: cached, fresh: fresh, invalid: make(map[string]bool)}
	s.w.Health = s.h
	// The main probe last worked on the active server a minute before the
	// monitor saw it die, so its dead is proof (diedAfterProbeOK).
	s.w.probeOKAt, s.w.probeOKActive = s.f.now.Add(-time.Minute), activeID(active)
}

// passThenMiss runs a tick whose main probe passes on the active server, has
// the monitor see every active endpoint take its status died after that tick,
// and leaves the main probe failing - until a switch writes another server -
// for a tick ProbeInterval later.
func (s *fastFixture) passThenMiss(died time.Duration) {
	healthy := true
	s.w.Probe = func(context.Context, int) error {
		s.probes++
		s.events = append(s.events, "probe")
		if healthy || len(s.written) > 0 {
			return nil
		}
		return errProbe
	}
	s.w.probeOKAt, s.w.probeOKActive = time.Time{}, ""
	s.w.Tick(context.Background())
	for _, key := range s.activeKeys() {
		for _, endpoints := range []map[string]watchdapi.EndpointState{s.h.cached.Endpoints, s.h.fresh.Endpoints} {
			state := endpoints[key]
			state.Since = s.f.now.Add(died)
			endpoints[key] = state
		}
	}
	healthy = false
	s.f.now = s.f.now.Add(ProbeInterval)
}

func (s *fastFixture) activeKeys() []string { return endpoint.Keys(cloneSubs(s.f.subs)[0].Servers[0]) }
func (s *fastFixture) candidateKey() string {
	return endpoint.Keys(cloneSubs(s.f.subs)[0].Servers[1])[0]
}

func (s *fastFixture) assertNoMutation(t *testing.T) {
	t.Helper()
	if s.generateCalls != 0 || s.restarts != 0 || s.f.applies != 0 || s.updates != 0 || s.saves != 0 || len(s.f.notes) != 0 {
		t.Fatalf("generate %d, restart %d, apply %d, update %d, save %d, notes %v before legacy confirmation", s.generateCalls, s.restarts, s.f.applies, s.updates, s.saves, s.f.notes)
	}
}

func assertFastAssignments(t *testing.T, cfg *vpnconfig.VPNDirectorConfig) {
	t.Helper()
	if !reflect.DeepEqual(cfg.Xray.Clients, []string{"192.168.1.8", "192.168.1.9"}) ||
		!reflect.DeepEqual(cfg.PausedClients, []string{"192.168.1.9"}) ||
		!reflect.DeepEqual(cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, []string{"192.168.1.3"}) || cfg.Xray.Failover != nil || cfg.Xray.PendingRestore != nil {
		t.Fatalf("direct switch changed client routing: Xray %v, paused %v, tunnels %v, failover %+v, pending %+v", cfg.Xray.Clients, cfg.PausedClients, cfg.TunnelDirector.Tunnels, cfg.Xray.Failover, cfg.Xray.PendingRestore)
	}
}

func TestFast_FirstFailureSwitchesWithoutClientMove(t *testing.T) {
	s := newFastFixture(t)
	s.w.Tick(context.Background())
	if s.restarts != 1 || s.f.applies != 0 || s.generateCalls != 1 || s.probes != 2 {
		t.Fatalf("restart %d, apply %d, generate %d, probes %d; the first failed main probe must switch directly", s.restarts, s.f.applies, s.generateCalls, s.probes)
	}
	assertFastAssignments(t, s.f.cfg)
	if !reflect.DeepEqual(s.f.cfg.Xray.Servers, []string{"203.0.113.10", "203.0.113.11", "198.51.100.20"}) {
		t.Fatalf("TPROXY bypass endpoints changed: %v", s.f.cfg.Xray.Servers)
	}
	wantActive := &vpnconfig.ActiveServer{Subscription: "alpha", Name: "Backup", Address: "backup.example", Port: 443, Seq: 8}
	wantPreferred := &vpnconfig.ActiveServer{Subscription: "alpha", Name: "Oslo", Address: "oslo.example", Port: 443}
	if !reflect.DeepEqual(s.f.cfg.Xray.ActiveServer, wantActive) || !reflect.DeepEqual(s.f.cfg.Xray.PreferredServer, wantPreferred) {
		t.Fatalf("active %+v, preferred %+v; automatic selection must preserve the original choice", s.f.cfg.Xray.ActiveServer, s.f.cfg.Xray.PreferredServer)
	}
	if len(s.written) != 1 || !reflect.DeepEqual(s.written[0].IPs, []string{"198.51.100.20"}) || endpoint.ServerForDial(s.written[0]).Address != "198.51.100.20" {
		t.Fatalf("generated identities %+v; the fresh candidate's checked IPv4 must be dialed", s.written)
	}
	if !reflect.DeepEqual(s.events, []string{"probe", "generate Backup", "restart", "settle", "probe"}) {
		t.Fatalf("main-process transition %v", s.events)
	}
	if len(s.f.notes) != 1 || !strings.Contains(s.f.notes[0], "Backup") || strings.Contains(s.f.notes[0], "back on Xray") || strings.Contains(s.f.notes[0], "moved to") {
		t.Fatalf("notes %v; want one server-change event, not a client restoration", s.f.notes)
	}
	if !s.w.failSince.IsZero() || s.w.returnNotBefore.Before(s.f.now.Add(5*time.Minute)) {
		t.Fatalf("failure clock %v, preferred return %v; the working switch must settle and keep the five-minute return delay", s.w.failSince, s.w.returnNotBefore)
	}
	s.f.now = s.f.now.Add(30 * time.Second)
	s.w.Tick(context.Background())
	if s.restarts != 1 || s.f.applies != 0 || len(s.f.notes) != 1 {
		t.Fatalf("healthy next tick repeated a switch: restart %d, apply %d, notes %v", s.restarts, s.f.applies, s.f.notes)
	}
}

// The monitor dials the stored IPs over plain HTTP where main Xray may dial a
// hostname, and can show a working server dead for days. A death it saw before
// the main probe last passed on that server proves nothing: one miss neither
// switches nor falls back, and the legacy confirmation moves the clients after
// the full three minutes.
func TestFast_ADeathBeforeTheLastWorkingProbeIsNoProof(t *testing.T) {
	s := newFastFixture(t)
	kinds := fastLogKinds(t)
	s.passThenMiss(-24 * time.Hour)
	start := s.f.now
	s.w.Tick(context.Background())
	if len(s.h.requests()) == 0 {
		t.Fatal("the miss did not ask the monitor for fresh evidence")
	}
	s.assertNoMutation(t)
	if s.f.cfg.Xray.Failover != nil || !s.w.failSince.Equal(start) {
		t.Fatalf("failover %+v, failSince %v; the legacy confirmation must go on from the miss", s.f.cfg.Xray.Failover, s.w.failSince)
	}
	if got := kinds(); !reflect.DeepEqual(got, []string{"evidence"}) {
		t.Fatalf("fast attempt records %v, want one inconclusive attempt of kind evidence", got)
	}
	for _, elapsed := range []time.Duration{30 * time.Second, DeadAfter - time.Second} {
		s.f.now = start.Add(elapsed)
		s.w.Tick(context.Background())
		s.assertNoMutation(t)
	}
	s.f.now = start.Add(DeadAfter)
	s.w.Tick(context.Background())
	if !vpnconfig.FailoverCommitted(s.f.cfg) || s.f.applies != 2 || s.generateCalls != 0 || s.restarts != 0 {
		t.Fatalf("failover %+v, apply %d, generate %d, restart %d; the legacy confirmation decides after three minutes", s.f.cfg.Xray.Failover, s.f.applies, s.generateCalls, s.restarts)
	}
	assertStillOnTunnel(t, s.f.cfg)
}

// A death the monitor saw after the main probe last passed on the active server
// is proof: the first miss switches Xray at once.
func TestFast_ADeathAfterTheLastWorkingProbeSwitches(t *testing.T) {
	s := newFastFixture(t)
	s.passThenMiss(10 * time.Second)
	s.w.Tick(context.Background())
	if s.generateCalls != 1 || s.restarts != 1 || s.f.applies != 0 || s.f.cfg.Xray.ActiveServer.Name != "Backup" {
		t.Fatalf("generate %d, restart %d, apply %d, active %+v; the first miss must switch directly", s.generateCalls, s.restarts, s.f.applies, s.f.cfg.Xray.ActiveServer)
	}
	assertFastAssignments(t, s.f.cfg)
	if len(s.f.notes) != 1 || !strings.Contains(s.f.notes[0], "Backup") {
		t.Fatalf("notes %v; want one server-change event", s.f.notes)
	}
	if !s.w.failSince.IsZero() {
		t.Fatalf("failSince %v; the working switch must settle", s.w.failSince)
	}
}

// The same proof with no candidate alive falls back to the tunnel at once.
func TestFast_ADeathAfterTheLastWorkingProbeFallsBack(t *testing.T) {
	s := newFastFixture(t)
	state := s.h.fresh.Endpoints[s.candidateKey()]
	state.Status = watchdapi.StatusDead
	s.h.fresh.Endpoints[s.candidateKey()] = state
	s.passThenMiss(10 * time.Second)
	s.w.Tick(context.Background())
	if !vpnconfig.FailoverCommitted(s.f.cfg) || s.f.applies != 2 || s.generateCalls != 0 || s.restarts != 0 {
		t.Fatalf("failover %+v, apply %d, generate %d, restart %d; the first miss must fall back to the tunnel", s.f.cfg.Xray.Failover, s.f.applies, s.generateCalls, s.restarts)
	}
	assertStillOnTunnel(t, s.f.cfg)
	if countNotes(s.f.notes, "Xray outbound is down; LAN clients moved") != 1 {
		t.Fatalf("notes %v; want one move to the tunnel", s.f.notes)
	}
}

// With no working main probe of the active server on record - none yet, or the
// last one passed on another server - the monitor's dead proves nothing.
func TestFast_NoWorkingProbeOfTheActiveServerIsNoProof(t *testing.T) {
	for _, tc := range []struct {
		name   string
		worked *vpnconfig.ActiveServer // the server the last working probe ran on; nil for none
	}{
		{"no working probe yet", nil},
		{"working probe of another server", &vpnconfig.ActiveServer{Subscription: "alpha", Name: "Backup", Address: "backup.example", Port: 443, Seq: 6}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newFastFixture(t)
			kinds := fastLogKinds(t)
			s.w.probeOKAt, s.w.probeOKActive = time.Time{}, ""
			if tc.worked != nil {
				s.w.probeOKAt, s.w.probeOKActive = s.f.now.Add(-time.Minute), activeID(tc.worked)
			}
			s.w.Tick(context.Background())
			if len(s.h.requests()) == 0 {
				t.Fatal("the miss did not ask the monitor for fresh evidence")
			}
			s.assertNoMutation(t)
			if s.f.cfg.Xray.Failover != nil || !s.w.failSince.Equal(s.f.now) {
				t.Fatalf("failover %+v, failSince %v; the legacy confirmation must go on from the miss", s.f.cfg.Xray.Failover, s.w.failSince)
			}
			if got := kinds(); !reflect.DeepEqual(got, []string{"evidence"}) {
				t.Fatalf("fast attempt records %v, want one inconclusive attempt of kind evidence", got)
			}
		})
	}
}

func TestFast_OrderDedupAndBudget(t *testing.T) {
	s := newFastFixture(t)
	active := s.f.subs[0].Servers[0]
	alias := active
	alias.Name = "Alias"
	nearby := fastServer("Nearby", "nearby.example", "198.51.100.20")
	nearby.IPs = []string{"198.51.100.20", "198.51.100.20", "198.51.100.21"}
	beta := fastServer("Beta", "beta.example", "192.0.2.30")
	gamma := fastServer("Gamma", "gamma.example", "192.0.2.40")
	s.f.subs = []vpnconfig.Subscription{
		{ID: "gamma", Name: "Gamma", URL: "https://gamma.example/list", Servers: []vpnconfig.Server{gamma}},
		{ID: "alpha", Name: "Alpha", URL: "https://alpha.example/list", Servers: []vpnconfig.Server{active, alias, nearby, fastServer("Overflow", "overflow.example", "198.51.100.22")}},
		{ID: "beta", Name: "Beta", URL: "https://beta.example/list", Servers: []vpnconfig.Server{beta}},
	}
	s.setHealth()
	for _, key := range endpoint.Keys(active) {
		state := s.h.cached.Endpoints[key]
		state.Status, state.LatencyMS, state.Fails, state.Error = watchdapi.StatusAlive, 17, 0, ""
		s.h.cached.Endpoints[key] = state
		state = s.h.fresh.Endpoints[key]
		state.Status, state.LatencyMS, state.Fails, state.Error = watchdapi.StatusDead, 0, 1, "timeout"
		s.h.fresh.Endpoints[key] = state
	}
	release := make(chan struct{})
	var released sync.Once
	unblock := func() { released.Do(func() { close(release) }) }
	entered := make(chan fastCheckCall, 12)
	block := func(ctx context.Context, keys []string) {
		deadline, bounded := ctx.Deadline()
		entered <- fastCheckCall{append([]string(nil), keys...), time.Now(), deadline, bounded}
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	s.h.check = func(ctx context.Context, keys []string) (monitor.Evidence, error) {
		block(ctx, keys)
		return copyFastEvidence(s.h.fresh, keys), ctx.Err()
	}
	s.w.WANUp = func(ctx context.Context) bool { block(ctx, nil); return ctx.Err() == nil }
	s.w.Probe = func(context.Context, int) error {
		s.probes++
		if len(s.written) == 3 {
			return nil
		}
		return errProbe
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() { unblock(); cancel(); awaitMutation(t, done, "parallel fast tick shutdown") })
	go func() { defer close(done); s.w.Tick(ctx) }()
	var calls []fastCheckCall
	for range 5 {
		select {
		case call := <-entered:
			calls = append(calls, call)
		case <-time.After(3 * time.Second):
			t.Fatal("active set, three individual candidates and WAN must all start before any result is released")
		}
	}
	unblock()
	awaitMutation(t, done, "bounded fast switch")
	activeKeys := endpoint.Keys(active)
	wantCandidates := map[string]bool{
		endpoint.Key(endpoint.PerAddress([]vpnconfig.Server{nearby})[0]): true,
		endpoint.Key(endpoint.PerAddress([]vpnconfig.Server{nearby})[2]): true,
		endpoint.Key(beta): true,
	}
	seen, activeCalls, controls := make(map[string]bool), 0, 0
	var shared time.Time
	for _, call := range calls {
		if !call.bounded || call.deadline.Sub(call.started) <= 0 || call.deadline.Sub(call.started) > 30*time.Second {
			t.Errorf("additional check deadline %v after %v (bounded %v), want at most 30 seconds", call.deadline, call.started, call.bounded)
		}
		if shared.IsZero() {
			shared = call.deadline
		} else if !shared.Equal(call.deadline) {
			t.Errorf("check deadlines differ: %v and %v", shared, call.deadline)
		}
		if call.keys == nil {
			controls++
			continue
		}
		if len(call.keys) == 2 {
			got, want := append([]string(nil), call.keys...), append([]string(nil), activeKeys...)
			sort.Strings(got)
			sort.Strings(want)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("active request %v, want every active key %v", got, want)
			}
			activeCalls++
			continue
		}
		if len(call.keys) != 1 || !wantCandidates[call.keys[0]] || seen[call.keys[0]] {
			t.Fatalf("candidate request %v; exclude active keys, deduplicate aliases and stop after three", call.keys)
		}
		seen[call.keys[0]] = true
	}
	if activeCalls != 1 || controls != 1 || len(seen) != 3 || len(s.h.requests()) != 4 {
		t.Fatalf("active %d, WAN %d, distinct candidates %d, CheckEvidence calls %d", activeCalls, controls, len(seen), len(s.h.requests()))
	}
	var dialed []string
	for _, server := range s.written {
		dialed = append(dialed, server.Subscription+"/"+server.Name+"/"+endpoint.ServerForDial(server).Address)
	}
	if !reflect.DeepEqual(dialed, []string{"alpha/Nearby/198.51.100.20", "alpha/Nearby/198.51.100.21", "beta/Beta/192.0.2.30"}) || s.restarts != 3 || s.f.applies != 0 {
		t.Fatalf("dialed %v, restart %d, apply %d; preserve OwnFirst/walkOrder/PerAddress despite parallel completion", dialed, s.restarts, s.f.applies)
	}
	assertFastAssignments(t, s.f.cfg)
}

func TestFast_InconclusiveUsesLegacy(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*fastFixture)
	}{
		{"one active address alive", func(s *fastFixture) {
			key := s.activeKeys()[1]
			state := s.h.fresh.Endpoints[key]
			state.Status = watchdapi.StatusAlive
			s.h.fresh.Endpoints[key] = state
		}},
		{"one active address missing", func(s *fastFixture) { delete(s.h.fresh.Endpoints, s.activeKeys()[1]) }},
		{"empty fresh results", func(s *fastFixture) { s.h.fresh.Endpoints = nil }},
		{"active rejected", func(s *fastFixture) {
			key := s.activeKeys()[0]
			state := s.h.fresh.Endpoints[key]
			state.Status = watchdapi.StatusRejected
			s.h.fresh.Endpoints[key] = state
		}},
		{"active unknown", func(s *fastFixture) {
			key := s.activeKeys()[0]
			state := s.h.fresh.Endpoints[key]
			state.Status = watchdapi.StatusUnknown
			s.h.fresh.Endpoints[key] = state
		}},
		{"candidate missing after cached alive", func(s *fastFixture) { delete(s.h.fresh.Endpoints, s.candidateKey()) }},
		{"WAN unavailable", func(s *fastFixture) { s.w.WANUp = func(context.Context) bool { return false } }},
		{"nil WAN", func(s *fastFixture) { s.w.WANUp = nil }},
		{"nil monitor", func(s *fastFixture) { s.w.Health = nil }},
		{"monitor disabled", func(s *fastFixture) {
			s.h.cached.State, s.h.fresh.State = watchdapi.StateDisabled, watchdapi.StateDisabled
		}},
		{"monitor stopped", func(s *fastFixture) {
			s.h.cached.State, s.h.fresh.State = watchdapi.StateStopped, watchdapi.StateStopped
		}},
		{"monitor WAN pause", func(s *fastFixture) {
			s.h.cached.State, s.h.fresh.State = watchdapi.StateWANDown, watchdapi.StateWANDown
		}},
		{"monitor has no Xray", func(s *fastFixture) { s.h.cached.State, s.h.fresh.State = watchdapi.StateNoXray, watchdapi.StateNoXray }},
		{"prober crash", func(s *fastFixture) {
			s.h.check = func(context.Context, []string) (monitor.Evidence, error) {
				return monitor.Evidence{State: watchdapi.StateProberError}, errors.New("synthetic prober exit")
			}
		}},
		{"fresh check timeout", func(s *fastFixture) {
			s.h.check = func(_ context.Context, keys []string) (monitor.Evidence, error) {
				return copyFastEvidence(s.h.fresh, keys), context.DeadlineExceeded
			}
		}},
		{"fresh active generation invalidated", func(s *fastFixture) { s.h.invalidate(s.activeKeys()[0]) }},
		{"fresh candidate generation invalidated", func(s *fastFixture) { s.h.invalidate(s.candidateKey()) }},
		{"missing active selection", func(s *fastFixture) { s.f.cfg.Xray.ActiveServer = nil }},
		{"same name but rotated active address", func(s *fastFixture) { s.f.cfg.Xray.ActiveServer.Address = "rotated.example" }},
		{"no matching active subscription", func(s *fastFixture) { s.f.cfg.Xray.ActiveServer.Subscription = "missing" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newFastFixture(t)
			tc.setup(s)
			before, err := cloneCfg(s.f.cfg)
			if err != nil {
				t.Fatal(err)
			}
			s.w.Tick(context.Background())
			s.assertNoMutation(t)
			if !reflect.DeepEqual(s.f.cfg, before) {
				t.Fatal("an inconclusive monitor answer changed configuration before the legacy timer")
			}
			if s.probes != 1 {
				t.Fatalf("main probes %d, want one ordinary probe without a busy retry loop", s.probes)
			}
		})
	}
	t.Run("healthy main HTTPS wins over monitor death", func(t *testing.T) {
		s := newFastFixture(t)
		s.w.Probe = func(context.Context, int) error { s.probes++; return nil }
		s.w.Tick(context.Background())
		s.assertNoMutation(t)
		if len(s.h.requests()) != 0 || s.wanCalls.Load() != 0 || s.probes != 1 {
			t.Fatalf("fresh checks %d, WAN %d, main probes %d; a healthy main HTTPS probe must not trigger confirmation", len(s.h.requests()), s.wanCalls.Load(), s.probes)
		}
	})
	for _, tc := range []struct {
		name       string
		tcp        bool
		before, at time.Duration
	}{
		{"ordinary three-minute timer", false, 3*time.Minute - time.Second, 3 * time.Minute},
		{"disabled monitor retains TCP one-minute timer", true, time.Minute - time.Second, time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newFastFixture(t)
			s.h.cached.State, s.h.fresh.State = watchdapi.StateDisabled, watchdapi.StateDisabled
			if tc.tcp {
				s.w.Reachable = func(_ context.Context, ip string, _ int) bool { return ip == "1.1.1.1" }
			}
			start := s.f.now
			for _, elapsed := range []time.Duration{0, 30 * time.Second, tc.before} {
				s.f.now = start.Add(elapsed)
				s.w.Tick(context.Background())
				s.assertNoMutation(t)
			}
			s.f.now = start.Add(tc.at)
			s.w.Tick(context.Background())
			if !vpnconfig.FailoverCommitted(s.f.cfg) || s.f.applies != 2 || s.generateCalls != 0 || s.restarts != 0 {
				t.Fatalf("legacy boundary %v: failover %+v, apply %d, generate %d, restart %d", tc.at, s.f.cfg.Xray.Failover, s.f.applies, s.generateCalls, s.restarts)
			}
			assertStillOnTunnel(t, s.f.cfg)
		})
	}
	t.Run("parent timeout drains concurrent checks without mutation", func(t *testing.T) {
		s := newFastFixture(t)
		var finished atomic.Int32
		s.h.check = func(ctx context.Context, _ []string) (monitor.Evidence, error) {
			<-ctx.Done()
			finished.Add(1)
			return monitor.Evidence{}, ctx.Err()
		}
		s.w.WANUp = func(ctx context.Context) bool { <-ctx.Done(); finished.Add(1); return false }
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		s.w.Tick(ctx)
		s.assertNoMutation(t)
		if len(s.h.requests()) != 2 || finished.Load() != 3 {
			t.Fatalf("requests %d, finished %d; active, candidate and WAN checks must be joined on timeout", len(s.h.requests()), finished.Load())
		}
	})
}

// fastLogKinds captures the log for the rest of the test and returns, when
// called, the error kind of every record saying the fast attempt did not switch.
func fastLogKinds(t *testing.T) func() []string {
	t.Helper()
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return func() []string {
		t.Helper()
		kinds := []string{}
		decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
		for {
			var record struct {
				Level   string `json:"level"`
				Message string `json:"msg"`
				Error   struct {
					Kind string `json:"kind"`
				} `json:"error"`
			}
			if err := decoder.Decode(&record); errors.Is(err, io.EOF) {
				return kinds
			} else if err != nil {
				t.Fatal(err)
			}
			if record.Message == "Fast failover did not switch; confirming the failure" {
				if record.Level != "INFO" {
					t.Fatalf("fast attempt logged at %s, want INFO", record.Level)
				}
				kinds = append(kinds, record.Error.Kind)
			}
		}
	}
}

// A fast attempt that leaves the death to the legacy confirmation says so, and
// why, once a failure episode.
func TestFast_InconclusiveAttemptLogsOncePerEpisode(t *testing.T) {
	t.Run("stale evidence", func(t *testing.T) {
		s := newFastFixture(t)
		kinds := fastLogKinds(t)
		s.h.invalidate(s.activeKeys()[0])
		start := s.f.now
		for _, elapsed := range []time.Duration{0, 30 * time.Second, time.Minute} {
			s.f.now = start.Add(elapsed)
			s.w.Tick(context.Background())
		}
		s.assertNoMutation(t)
		if got := kinds(); !reflect.DeepEqual(got, []string{"evidence"}) {
			t.Fatalf("fast attempt records %v, want one with kind evidence for the episode", got)
		}
		healthy := true
		s.w.Probe = func(context.Context, int) error {
			s.probes++
			if healthy {
				return nil
			}
			return errProbe
		}
		s.f.now = start.Add(90 * time.Second)
		s.w.Tick(context.Background())
		healthy = false
		s.f.now = start.Add(2 * time.Minute)
		s.w.Tick(context.Background())
		if got := kinds(); !reflect.DeepEqual(got, []string{"evidence", "evidence"}) {
			t.Fatalf("fast attempt records %v, want one more for the new episode", got)
		}
	})
	for _, tc := range []struct {
		name  string
		setup func(*fastFixture)
	}{
		{"monitor disabled", func(s *fastFixture) {
			s.h.cached.State, s.h.fresh.State = watchdapi.StateDisabled, watchdapi.StateDisabled
		}},
		{"no monitor", func(s *fastFixture) { s.w.Health = nil }},
	} {
		t.Run("not applicable/"+tc.name, func(t *testing.T) {
			s := newFastFixture(t)
			kinds := fastLogKinds(t)
			tc.setup(s)
			s.w.Tick(context.Background())
			s.assertNoMutation(t)
			if got := kinds(); !reflect.DeepEqual(got, []string{"unavailable"}) {
				t.Fatalf("fast attempt records %v, want one with kind unavailable", got)
			}
		})
	}
	for _, outcome := range []string{"switched", "fallback"} {
		t.Run(outcome+" attempt logs nothing", func(t *testing.T) {
			s := newFastFixture(t)
			kinds := fastLogKinds(t)
			if outcome == "fallback" {
				state := s.h.fresh.Endpoints[s.candidateKey()]
				state.Status = watchdapi.StatusDead
				s.h.fresh.Endpoints[s.candidateKey()] = state
			}
			s.w.Tick(context.Background())
			if outcome == "switched" && (s.restarts != 1 || s.f.cfg.Xray.ActiveServer.Name != "Backup") {
				t.Fatalf("restart %d, active %+v; the fixture must switch", s.restarts, s.f.cfg.Xray.ActiveServer)
			}
			if outcome == "fallback" && s.f.cfg.Xray.Failover == nil {
				t.Fatal("the fixture must fall back to the tunnel")
			}
			if got := kinds(); len(got) != 0 {
				t.Fatalf("fast attempt records %v after a %s attempt", got, outcome)
			}
		})
	}
}

func TestFast_StopAndCompatibilityDrainFreshChecks(t *testing.T) {
	defer func(poll time.Duration) { stopPoll = poll }(stopPoll)
	stopPoll = time.Millisecond
	for _, refusal := range []string{"stop", "compatibility"} {
		t.Run(refusal, func(t *testing.T) {
			s := newFastFixture(t)
			var stopped, incompatible atomic.Bool
			s.w.Stopped, s.w.CanMutate = stopped.Load, mutationGate(&incompatible)
			entered := make(chan struct{}, 3)
			var finished atomic.Int32
			wait := func(ctx context.Context) {
				entered <- struct{}{}
				<-ctx.Done()
				finished.Add(1)
			}
			s.h.check = func(ctx context.Context, _ []string) (monitor.Evidence, error) {
				wait(ctx)
				return monitor.Evidence{}, ctx.Err()
			}
			s.w.WANUp = func(ctx context.Context) bool { wait(ctx); return false }
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			t.Cleanup(func() { cancel(); awaitMutation(t, done, "refused fast checks shutdown") })
			go func() { defer close(done); s.w.Tick(ctx) }()
			for range 3 {
				awaitMutation(t, entered, "active, candidate and WAN checks starting")
			}
			if refusal == "stop" {
				stopped.Store(true)
			} else {
				incompatible.Store(true)
			}
			awaitMutation(t, done, "mutation gate canceling all fresh checks")
			s.assertNoMutation(t)
			assertFastAssignments(t, s.f.cfg)
			if finished.Load() != 3 || !s.w.failSince.IsZero() {
				t.Fatalf("refused checks finished %d, failSince %v; drain workers and reset stopped/incompatible confirmation", finished.Load(), s.w.failSince)
			}
		})
	}
}

func TestFast_CompletedCandidateSurvivesIncompletePeer(t *testing.T) {
	s := newFastFixture(t)
	s.f.subs[0].Servers = append(s.f.subs[0].Servers, fastServer("Extra", "extra.example", "192.0.2.30"))
	s.setHealth()
	delete(s.h.fresh.Endpoints, s.candidateKey())
	s.w.Tick(context.Background())
	if len(s.written) != 1 || s.written[0].Name != "Extra" || s.restarts != 1 || s.f.applies != 0 || len(s.f.notes) != 1 {
		t.Fatalf("usable fresh candidate was lost to an incomplete peer: written %+v, restart %d, apply %d, notes %v", s.written, s.restarts, s.f.applies, s.f.notes)
	}
	assertFastAssignments(t, s.f.cfg)
}

func TestFast_InfrastructureFailureKeepsLegacyClock(t *testing.T) {
	for _, cause := range []error{service.ErrConfigLockTimeout, errors.Join(service.ErrConfigLoad, os.ErrNotExist)} {
		t.Run(cause.Error(), func(t *testing.T) {
			s := newFastFixture(t)
			s.w.Generate = func(_ vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
				s.generateCalls++
				if err := s.f.checkGuard(guard); err != nil {
					return false, s.f.seq(), err
				}
				return false, s.f.seq(), cause
			}
			start := s.f.now
			for _, elapsed := range []time.Duration{0, 30 * time.Second} {
				s.f.now = start.Add(elapsed)
				s.w.Tick(context.Background())
				if !s.w.failSince.Equal(start) || s.restarts != 0 || s.f.applies != 0 || s.saves != 0 || len(s.f.notes) != 0 {
					t.Fatalf("infrastructure error reset/finished confirmation: failSince %v, restart %d, apply %d, save %d, notes %v", s.w.failSince, s.restarts, s.f.applies, s.saves, s.f.notes)
				}
				assertFastAssignments(t, s.f.cfg)
			}
			if s.generateCalls != 2 {
				t.Fatalf("Generate calls %d; retry once per normal tick, never in a busy loop", s.generateCalls)
			}
		})
	}
}

func TestFast_GuardsAndFailureFallback(t *testing.T) {
	for _, change := range []string{"stop", "compatibility", "same-server seq", "active link deletion", "active link rotation", "candidate outbound", "active outbound", "candidate address", "active evidence", "candidate evidence"} {
		t.Run("guard under Generate lock/"+change, func(t *testing.T) {
			s := newFastFixture(t)
			var stopped, incompatible atomic.Bool
			s.w.Stopped, s.w.CanMutate = stopped.Load, mutationGate(&incompatible)
			generate := s.w.Generate
			entered := 0
			s.w.Generate = func(server vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
				entered++
				switch change {
				case "stop":
					stopped.Store(true)
				case "compatibility":
					incompatible.Store(true)
				case "same-server seq":
					s.f.cfg.Xray.ActiveServer = vpnconfig.RecordActiveServer(s.f.cfg.Xray.ActiveServer, s.f.subs[0].Servers[0])
				case "active link deletion":
					s.f.subs = nil
					s.f.noSubs = true
				case "active link rotation":
					s.f.subs[0].URL = "https://alpha.example/rotated"
				case "candidate outbound":
					s.f.subs[0].Servers[1].Outbound = json.RawMessage(strings.ReplaceAll(string(s.f.subs[0].Servers[1].Outbound), "000000000010", "000000000011"))
				case "active outbound":
					s.f.subs[0].Servers[0].Outbound = json.RawMessage(strings.ReplaceAll(string(s.f.subs[0].Servers[0].Outbound), "000000000010", "000000000011"))
				case "candidate address":
					s.f.subs[0].Servers[1].IPs = []string{"198.51.100.21"}
				case "active evidence":
					s.h.invalidate(s.activeKeys()[0])
				case "candidate evidence":
					s.h.invalidate(s.candidateKey())
				}
				return generate(server, guard)
			}
			beforeClients := append([]string(nil), s.f.cfg.Xray.Clients...)
			s.w.Tick(context.Background())
			wantSeq := 7
			if change == "same-server seq" {
				wantSeq = 8
			}
			if entered != 1 || len(s.written) != 0 || s.restarts != 0 || s.f.applies != 0 || len(s.f.notes) != 0 || s.f.cfg.Xray.ActiveServer.Name != "Oslo" || s.f.seq() != wantSeq || !reflect.DeepEqual(s.f.cfg.Xray.Clients, beforeClients) {
				t.Fatalf("guard %s: entered %d, written %+v, restart %d, apply %d, notes %v, active %+v", change, entered, s.written, s.restarts, s.f.applies, s.f.notes, s.f.cfg.Xray.ActiveServer)
			}
		})
	}
	for _, owner := range []string{"active", "candidate"} {
		for _, change := range []string{"deletion", "link rotation"} {
			t.Run("independent subscription guard/"+owner+"/"+change, func(t *testing.T) {
				s := newFastFixture(t)
				active, backup := s.f.subs[0].Servers[0], s.f.subs[0].Servers[1]
				s.f.subs = []vpnconfig.Subscription{
					{ID: "alpha", Name: "Alpha", URL: "https://alpha.example/list", Servers: []vpnconfig.Server{active}},
					{ID: "beta", Name: "Beta", URL: "https://beta.example/list", Servers: []vpnconfig.Server{backup}},
				}
				s.setHealth()
				generate := s.w.Generate
				entered := 0
				s.w.Generate = func(server vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
					entered++
					if server.Subscription != "beta" {
						t.Errorf("candidate identity %s/%s, want beta/Backup", server.Subscription, server.Name)
					}
					index := 0
					if owner == "candidate" {
						index = 1
					}
					if change == "deletion" {
						s.f.subs = append(s.f.subs[:index], s.f.subs[index+1:]...)
					} else {
						s.f.subs[index].URL = "https://rotated.example/list"
					}
					return generate(server, guard)
				}
				s.w.Tick(context.Background())
				if entered != 1 || len(s.written) != 0 || s.restarts != 0 || s.f.applies != 0 || len(s.f.notes) != 0 || s.f.cfg.Xray.ActiveServer.Name != "Oslo" || s.f.seq() != 7 {
					t.Fatalf("%s %s guard: entered %d, written %+v, restart %d, apply %d, notes %v, active %+v", owner, change, entered, s.written, s.restarts, s.f.applies, s.f.notes, s.f.cfg.Xray.ActiveServer)
				}
				assertFastAssignments(t, s.f.cfg)
			})
		}
	}
	t.Run("same-server seq after the real config lock wait", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		s, store, xray, output := newFastPersistedFixture(t)
		held, release, holderDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var once sync.Once
		unlock := func() { once.Do(func() { close(release) }) }
		ctx, cancel := context.WithCancel(context.Background())
		tickDone := make(chan struct{})
		started := false
		t.Cleanup(func() {
			unlock()
			cancel()
			awaitMutation(t, holderDone, "manual lock-holder shutdown")
			if started {
				awaitMutation(t, tickDone, "locked fast generation shutdown")
			}
		})
		holderErrors := make(chan error, 1)
		go func() {
			defer close(holderDone)
			holderErrors <- store.UpdateVPNConfig(func(cfg *vpnconfig.VPNDirectorConfig) error {
				close(held)
				<-release
				cfg.Xray.ActiveServer = vpnconfig.RecordActiveServer(cfg.Xray.ActiveServer, s.f.subs[0].Servers[0])
				return nil
			})
		}()
		awaitMutation(t, held, "the actual held config lock")
		waiting := &mutationWaitStore{ConfigStore: store, entered: make(chan struct{})}
		var results []mutationGeneration
		s.w.Generate = func(server vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
			generated, seq, err := service.GenerateAndRecordGuardedWalkedServer(waiting, xray, endpoint.ServerForDial(server), server, service.InboundPorts{}, guard)
			results = append(results, mutationGeneration{generated, err})
			return generated, seq, err
		}
		started = true
		go func() { defer close(tickDone); s.w.Tick(ctx) }()
		awaitMutation(t, waiting.entered, "fast generation waiting for the lock")
		unlock()
		awaitMutation(t, holderDone, "manual re-selection committed")
		awaitMutation(t, tickDone, "same-lock ownership refusal")
		if err := <-holderErrors; err != nil {
			t.Fatal(err)
		}
		cfg, err := store.LoadVPNConfig()
		if err != nil {
			t.Fatal(err)
		}
		live, err := os.ReadFile(output)
		if err != nil || string(live) != "previous\n" || len(results) != 1 || results[0].generated || !errors.Is(results[0].err, errSuperseded) || cfg.Xray.ActiveServer.Name != "Oslo" || cfg.Xray.ActiveServer.Seq != 8 || s.restarts != 0 || s.f.applies != 0 || len(s.f.notes) != 0 {
			t.Fatalf("same-server manual selection was lost after flock: live %q, error %v, results %+v, active %+v, restart %d, apply %d, notes %v", live, err, results, cfg.Xray.ActiveServer, s.restarts, s.f.applies, s.f.notes)
		}
		assertFastAssignments(t, cfg)
	})
	for _, keyKind := range []string{"active", "candidate", "stop", "compatibility"} {
		t.Run("guard invalidated by real staged validation/"+keyKind, func(t *testing.T) {
			s, store, xray, output := newFastPersistedFixture(t)
			before, err := os.ReadFile(store.ConfigPath())
			if err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(t.TempDir(), "validated")
			bin := t.TempDir()
			script := fmt.Sprintf("#!/bin/sh\n[ \"$#\" -eq 6 ] && [ \"$1\" = run ] && [ \"$2\" = -test ] && [ \"$3\" = -format ] && [ \"$4\" = json ] && [ \"$5\" = -c ] && [ \"$6\" != %q ] || exit 94\nprintf 'validated\\n' > %q\n", output, marker)
			if err := os.WriteFile(filepath.Join(bin, "xray"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			invalidKey := s.activeKeys()[0]
			if keyKind == "candidate" {
				invalidKey = s.candidateKey()
			}
			validated := func() bool {
				_, err := os.Stat(marker)
				return err == nil
			}
			if keyKind == "stop" {
				s.w.Stopped = validated
			} else if keyKind == "compatibility" {
				s.w.CanMutate = func() error {
					if validated() {
						return watchcompat.ErrIncompatible
					}
					return nil
				}
			} else {
				s.h.validate = func(_ monitor.Evidence, keys []string) error {
					if validated() && contains(keys, invalidKey) {
						return errors.New("synthetic live generation invalidated during validation")
					}
					return nil
				}
			}
			var results []mutationGeneration
			s.w.Generate = func(server vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
				generated, seq, err := service.GenerateAndRecordGuardedWalkedServer(store, xray, endpoint.ServerForDial(server), server, service.InboundPorts{}, guard)
				results = append(results, mutationGeneration{generated, err})
				return generated, seq, err
			}
			s.w.Tick(context.Background())
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("real staged validation never ran: %v", err)
			}
			after, err := os.ReadFile(store.ConfigPath())
			if err != nil || string(after) != string(before) {
				t.Fatalf("selection/config changed after final %s evidence invalidation: %v", keyKind, err)
			}
			live, err := os.ReadFile(output)
			if err != nil || string(live) != "previous\n" || len(results) != 1 || results[0].generated || results[0].err == nil || s.restarts != 0 || s.f.applies != 0 || len(s.f.notes) != 0 {
				t.Fatalf("live %q, error %v, generation %+v, restart %d, apply %d, notes %v; revalidate both proofs immediately before rename", live, err, results, s.restarts, s.f.applies, s.f.notes)
			}
			stages, err := filepath.Glob(output + ".*")
			if err != nil || len(stages) != 0 {
				t.Fatalf("refused stage cleanup %v, error %v", stages, err)
			}
		})
	}
	for _, failure := range []string{"Generate", "restart", "main HTTPS"} {
		t.Run("next fresh candidate after "+failure, func(t *testing.T) {
			s := newFastFixture(t)
			s.f.subs[0].Servers = append(s.f.subs[0].Servers, fastServer("Extra", "extra.example", "192.0.2.30"))
			s.setHealth()
			generate := s.w.Generate
			var attempted []string
			s.w.Generate = func(server vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
				attempted = append(attempted, server.Name)
				if failure == "Generate" && server.Name == "Backup" {
					if err := s.f.checkGuard(guard); err != nil {
						return false, s.f.seq(), err
					}
					return false, s.f.seq(), errors.New("synthetic generation refusal")
				}
				return generate(server, guard)
			}
			restart := s.w.RestartXray
			s.w.RestartXray = func() error {
				err := restart()
				if failure == "restart" && s.restarts == 1 {
					return errApply
				}
				return err
			}
			s.w.Probe = func(context.Context, int) error {
				s.probes++
				if len(s.written) == 0 || (failure == "main HTTPS" && s.written[len(s.written)-1].Name == "Backup") {
					return errProbe
				}
				return nil
			}
			s.w.Tick(context.Background())
			wantRestarts := 2
			if failure == "Generate" {
				wantRestarts = 1
			}
			if !reflect.DeepEqual(attempted, []string{"Backup", "Extra"}) || s.f.cfg.Xray.ActiveServer.Name != "Extra" || s.restarts != wantRestarts || s.f.applies != 0 || len(s.f.notes) != 1 || strings.Contains(s.f.notes[0], "back on Xray") {
				t.Fatalf("failure %s: attempted %v, active %+v, restart %d, apply %d, notes %v", failure, attempted, s.f.cfg.Xray.ActiveServer, s.restarts, s.f.applies, s.f.notes)
			}
			assertFastAssignments(t, s.f.cfg)
		})
	}
	for _, key := range []string{"active", "candidate"} {
		t.Run("evidence stale after publication/"+key, func(t *testing.T) {
			s := newFastFixture(t)
			generate := s.w.Generate
			s.w.Generate = func(server vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
				generated, seq, err := generate(server, guard)
				if key == "active" {
					s.h.invalidate(s.activeKeys()[0])
				} else {
					s.h.invalidate(s.candidateKey())
				}
				return generated, seq, err
			}
			s.w.Tick(context.Background())
			if s.generateCalls != 1 || s.restarts != 1 || s.probes != 2 || s.f.applies != 0 || s.f.cfg.Xray.ActiveServer.Name != "Backup" {
				t.Fatalf("generate %d, restart %d, probes %d, apply %d, active %+v; a published switch must restart Xray and finish", s.generateCalls, s.restarts, s.probes, s.f.applies, s.f.cfg.Xray.ActiveServer)
			}
			if !reflect.DeepEqual(s.events, []string{"probe", "generate Backup", "restart", "settle", "probe"}) {
				t.Fatalf("main-process transition %v", s.events)
			}
			if len(s.f.notes) != 1 || !strings.Contains(s.f.notes[0], "Backup") {
				t.Fatalf("notes %v; want one server-change event", s.f.notes)
			}
			assertFastAssignments(t, s.f.cfg)
		})
	}
	for _, when := range []string{"restart", "settle", "main probe"} {
		t.Run("manual selection after "+when, func(t *testing.T) {
			s := newFastFixture(t)
			manual := func() {
				s.f.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Subscription: "alpha", Name: "Manual", Address: "manual.example", Port: 443, Seq: 9}
				s.f.cfg.Xray.PreferredServer = nil
			}
			s.w.RestartXray = func() error {
				s.restarts++
				if when == "restart" {
					manual()
				}
				return nil
			}
			s.w.AfterRestart = func(time.Duration) {
				if when == "settle" {
					manual()
				}
			}
			s.w.Probe = func(context.Context, int) error {
				s.probes++
				if s.probes == 1 {
					return errProbe
				}
				if when == "main probe" {
					manual()
				}
				return errProbe
			}
			s.w.Tick(context.Background())
			if len(s.written) != 1 || s.restarts != 1 || s.f.applies != 0 || s.saves != 0 || len(s.f.notes) != 0 || s.f.cfg.Xray.ActiveServer.Name != "Manual" || s.f.seq() != 9 {
				t.Fatalf("superseded after %s: written %+v, restart %d, apply %d, save %d, notes %v, active %+v", when, s.written, s.restarts, s.f.applies, s.saves, s.f.notes, s.f.cfg.Xray.ActiveServer)
			}
			assertFastAssignments(t, s.f.cfg)
		})
	}
	for _, ready := range []bool{true, false} {
		for _, failure := range []string{"no cached alive candidate", "fresh candidate dead", "Generate", "restart", "main HTTPS"} {
			t.Run(fmt.Sprintf("guarded tunnel fallback/%s/ready=%t", failure, ready), func(t *testing.T) {
				s := newFastFixture(t)
				key := s.candidateKey()
				if failure == "no cached alive candidate" {
					state := s.h.cached.Endpoints[key]
					state.Status = watchdapi.StatusDead
					s.h.cached.Endpoints[key] = state
				}
				if failure == "fresh candidate dead" {
					state := s.h.fresh.Endpoints[key]
					state.Status = watchdapi.StatusDead
					s.h.fresh.Endpoints[key] = state
				}
				if failure == "Generate" {
					s.w.Generate = func(_ vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
						s.generateCalls++
						if err := s.f.checkGuard(guard); err != nil {
							return false, s.f.seq(), err
						}
						return false, s.f.seq(), errProbe
					}
				}
				if failure == "restart" {
					s.w.RestartXray = func() error { s.restarts++; return errApply }
				}
				s.w.Probe = func(context.Context, int) error { s.probes++; return errProbe }
				s.w.FallbackReady = func(string) bool { return ready }
				var xrayAtApply [][]string
				apply := s.w.Apply
				s.w.Apply = func() error {
					xrayAtApply = append(xrayAtApply, append([]string(nil), s.f.cfg.Xray.Clients...))
					return apply()
				}
				s.w.Tick(context.Background())
				wantApplies := 1
				if ready {
					wantApplies = 2
				}
				if s.f.applies != wantApplies || len(xrayAtApply) != wantApplies || !contains(xrayAtApply[0], "192.168.1.8") {
					t.Fatalf("fallback applies %d, memberships %v; install the tunnel before dropping TPROXY membership", s.f.applies, xrayAtApply)
				}
				if ready {
					assertStillOnTunnel(t, s.f.cfg)
					if contains(xrayAtApply[1], "192.168.1.8") || countNotes(s.f.notes, "Xray outbound is down; LAN clients moved") != 1 {
						t.Fatalf("committed membership %v, notes %v", xrayAtApply, s.f.notes)
					}
				} else {
					assertStagedOnTunnel(t, s.f.cfg)
					if countNotes(s.f.notes, "Xray outbound is down; LAN clients moved") != 0 {
						t.Fatalf("unready tunnel falsely announced as carrying clients: %v", s.f.notes)
					}
				}
				if countNotes(s.f.notes, "LAN clients back on Xray") != 0 {
					t.Fatalf("unchanged Xray assignment falsely restored: %v", s.f.notes)
				}
				if !reflect.DeepEqual(s.f.cfg.PausedClients, []string{"192.168.1.9"}) || !contains(s.f.cfg.TunnelDirector.Tunnels["ovpnc2"].Clients, "192.168.1.3") {
					t.Fatal("fallback changed paused or foreign assignments")
				}
			})
		}
	}
	for _, stage := range []string{"stage", "commit", "refresh"} {
		t.Run("fallback ownership at "+stage, func(t *testing.T) {
			s := newFastFixture(t)
			state := s.h.cached.Endpoints[s.candidateKey()]
			state.Status = watchdapi.StatusDead
			s.h.cached.Endpoints[s.candidateKey()] = state
			manual := func() {
				s.f.cfg.Xray.ActiveServer = &vpnconfig.ActiveServer{Subscription: "alpha", Name: "Manual", Address: "manual.example", Port: 443, Seq: 8}
			}
			update := s.w.UpdateVPN
			writes := 0
			s.w.UpdateVPN = func(fn func(*vpnconfig.VPNDirectorConfig) error) error {
				writes++
				if (stage == "stage" && writes == 1) || (stage == "commit" && writes == 2) {
					manual()
				}
				return update(fn)
			}
			notesBeforeRefresh, fetches := -1, 0
			s.w.Fetch = func(context.Context, string) ([]vpnconfig.Server, error) {
				fetches++
				notesBeforeRefresh = len(s.f.notes)
				manual()
				return []vpnconfig.Server{fastServer("Fresh", "fresh.example", "192.0.2.50")}, nil
			}
			s.w.Tick(context.Background())
			if s.f.cfg.Xray.ActiveServer.Name != "Manual" || s.f.seq() != 8 || s.generateCalls != 0 || s.restarts != 0 || s.saves != 0 {
				t.Fatalf("fallback %s overwrote/adopted manual choice: active %+v, generate %d, restart %d, save %d", stage, s.f.cfg.Xray.ActiveServer, s.generateCalls, s.restarts, s.saves)
			}
			switch stage {
			case "stage":
				assertFastAssignments(t, s.f.cfg)
				if s.f.applies != 0 || fetches != 0 || len(s.f.notes) != 0 {
					t.Fatalf("superseded stage continued: apply %d, fetch %d, notes %v", s.f.applies, fetches, s.f.notes)
				}
			case "commit":
				assertStagedOnTunnel(t, s.f.cfg)
				if s.f.applies != 1 || fetches != 0 || len(s.f.notes) != 0 {
					t.Fatalf("superseded commit continued: apply %d, fetch %d, notes %v", s.f.applies, fetches, s.f.notes)
				}
			case "refresh":
				assertStillOnTunnel(t, s.f.cfg)
				if s.f.applies != 2 || fetches != 1 || notesBeforeRefresh < 0 || len(s.f.notes) != notesBeforeRefresh {
					t.Fatalf("superseded refresh continued: apply %d, fetch %d, notes %v", s.f.applies, fetches, s.f.notes)
				}
			}
		})
	}
}

func newFastPersistedFixture(t *testing.T) (*fastFixture, *service.ConfigService, *service.XrayService, string) {
	t.Helper()
	s := newFastFixture(t)
	s.f.subs[0].ID = "a1b2c3d4"
	s.f.subs = cloneSubs(s.f.subs)
	s.f.cfg.Xray.ActiveServer.Subscription = s.f.subs[0].ID
	s.setHealth()
	dir := t.TempDir()
	store := service.NewConfigService(dir, filepath.Join(dir, "data"))
	if err := vpnconfig.SaveVPNDirectorConfig(store.ConfigPath(), s.f.cfg); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateVPNConfig(func(*vpnconfig.VPNDirectorConfig) error {
		for _, sub := range s.f.subs {
			if err := store.SaveSubscription(sub); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	template, output := filepath.Join(dir, "template.json"), filepath.Join(dir, "config.json")
	if err := os.WriteFile(template, []byte(`{"inbounds":[],"outbounds":[],"routing":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, []byte("previous\n"), 0600); err != nil {
		t.Fatal(err)
	}
	xray := service.NewXrayService(template, output)
	s.w.LoadVPN, s.w.LoadSubscriptions, s.w.UpdateVPN, s.w.SaveSubscription = store.LoadVPNConfig, store.LoadSubscriptions, store.UpdateVPNConfig, store.SaveSubscription
	s.w.Generate = func(server vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
		s.generateCalls++
		generated, seq, err := service.GenerateAndRecordGuardedWalkedServer(store, xray, endpoint.ServerForDial(server), server, service.InboundPorts{}, guard)
		if generated {
			s.written = append(s.written, server)
		}
		return generated, seq, err
	}
	return s, store, xray, output
}

type fastSaveFailureStore struct{ service.ConfigStore }

func (s fastSaveFailureStore) UpdateVPNConfig(fn func(*vpnconfig.VPNDirectorConfig) error) error {
	return s.ConfigStore.UpdateVPNConfig(func(cfg *vpnconfig.VPNDirectorConfig) error {
		if err := fn(cfg); err != nil {
			return err
		}
		return errSaveConfig
	})
}

func TestFast_GeneratedButRecordSaveFailed(t *testing.T) {
	for _, behavior := range []string{"healthy unsaved switch", "continue with persisted seq", "same-server manual selection after unsaved switch"} {
		t.Run(behavior, func(t *testing.T) {
			t.Setenv("PATH", t.TempDir())
			s, store, xray, output := newFastPersistedFixture(t)
			if err := store.UpdateVPNConfig(func(*vpnconfig.VPNDirectorConfig) error {
				sub := s.f.subs[0]
				sub.Servers = append(sub.Servers, fastServer("Extra", "extra.example", "192.0.2.30"))
				s.f.subs[0] = sub
				return store.SaveSubscription(sub)
			}); err != nil {
				t.Fatal(err)
			}
			s.setHealth()
			var attempted []string
			var recordedSeqs []int
			s.w.Generate = func(server vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
				attempted = append(attempted, server.Name)
				var target service.ConfigStore = store
				if len(attempted) == 1 {
					target = fastSaveFailureStore{store}
				}
				generated, seq, err := service.GenerateAndRecordGuardedWalkedServer(target, xray, endpoint.ServerForDial(server), server, service.InboundPorts{}, guard)
				if generated {
					s.written = append(s.written, server)
				}
				recordedSeqs = append(recordedSeqs, seq)
				if len(attempted) == 1 && (!generated || seq != 7 || !errors.Is(err, errSaveConfig)) {
					t.Errorf("record-save failure generated=%t, seq=%d, error=%v; live config is written but persisted seq remains seven", generated, seq, err)
				}
				return generated, seq, err
			}
			s.w.Probe = func(context.Context, int) error {
				s.probes++
				if s.probes == 1 {
					return errProbe
				}
				if behavior == "same-server manual selection after unsaved switch" {
					if err := store.UpdateVPNConfig(func(cfg *vpnconfig.VPNDirectorConfig) error {
						cfg.Xray.ActiveServer = vpnconfig.RecordActiveServer(cfg.Xray.ActiveServer, vpnconfig.Server{Subscription: cfg.Xray.ActiveServer.Subscription, Name: "Oslo", Address: "oslo.example", Port: 443})
						cfg.Xray.PreferredServer = nil
						return nil
					}); err != nil {
						t.Error(err)
					}
					return errProbe
				}
				if behavior == "continue with persisted seq" && s.probes == 2 {
					return errProbe
				}
				return nil
			}
			s.w.Tick(context.Background())
			cfg, err := store.LoadVPNConfig()
			if err != nil {
				t.Fatal(err)
			}
			live, err := os.ReadFile(output)
			if err != nil || string(live) == "previous\n" || !json.Valid(live) {
				t.Fatalf("generated=true did not publish the live config: %q, error %v", live, err)
			}
			assertFastAssignments(t, cfg)
			if s.f.applies != 0 {
				t.Fatalf("applies %d; record bookkeeping must not move clients", s.f.applies)
			}
			switch behavior {
			case "healthy unsaved switch":
				if !reflect.DeepEqual(attempted, []string{"Backup"}) || s.restarts != 1 || cfg.Xray.ActiveServer.Name != "Oslo" || vpnconfig.ActiveSeq(cfg.Xray.ActiveServer) != 7 || len(s.f.notes) != 1 || !strings.Contains(s.f.notes[0], "Backup") {
					t.Fatalf("unsaved healthy switch: attempted %v, restart %d, active %+v, notes %v", attempted, s.restarts, cfg.Xray.ActiveServer, s.f.notes)
				}
			case "continue with persisted seq":
				if !reflect.DeepEqual(attempted, []string{"Backup", "Extra"}) || !reflect.DeepEqual(recordedSeqs, []int{7, 8}) || s.restarts != 2 || cfg.Xray.ActiveServer.Name != "Extra" || vpnconfig.ActiveSeq(cfg.Xray.ActiveServer) != 8 || len(s.f.notes) != 1 {
					t.Fatalf("unsaved record adopted the wrong seq: attempted %v, seqs %v, restart %d, active %+v, notes %v", attempted, recordedSeqs, s.restarts, cfg.Xray.ActiveServer, s.f.notes)
				}
			case "same-server manual selection after unsaved switch":
				if !reflect.DeepEqual(attempted, []string{"Backup"}) || s.restarts != 1 || cfg.Xray.ActiveServer.Name != "Oslo" || vpnconfig.ActiveSeq(cfg.Xray.ActiveServer) != 8 || cfg.Xray.PreferredServer != nil || len(s.f.notes) != 0 {
					t.Fatalf("foreign selection was adopted after generated=true/save failure: attempted %v, restart %d, active %+v, preferred %+v, notes %v", attempted, s.restarts, cfg.Xray.ActiveServer, cfg.Xray.PreferredServer, s.f.notes)
				}
			}
		})
	}
}

func TestFast_GuardedGenerationPortSnapshot(t *testing.T) {
	cases := []struct {
		name      string
		at        string
		changed   service.InboundPorts
		generated service.InboundPorts
		restarts  int
		settles   int
		probes    []int
		switched  bool
	}{
		{"updated during checks", "checks", service.InboundPorts{TProxy: 22345, Socks: 22346}, service.InboundPorts{TProxy: 22345, Socks: 22346}, 1, 1, []int{12346, 22346}, true},
		{"socks changed after generation", "generate", service.InboundPorts{TProxy: 12345, Socks: 22346}, service.InboundPorts{TProxy: 12345, Socks: 12346}, 0, 0, []int{12346}, false},
		{"tproxy changed after generation", "generate", service.InboundPorts{TProxy: 22345, Socks: 12346}, service.InboundPorts{TProxy: 12345, Socks: 12346}, 0, 0, []int{12346}, false},
		{"socks changed during restart", "restart", service.InboundPorts{TProxy: 12345, Socks: 22346}, service.InboundPorts{TProxy: 12345, Socks: 12346}, 1, 0, []int{12346}, false},
		{"tproxy changed during restart", "restart", service.InboundPorts{TProxy: 22345, Socks: 12346}, service.InboundPorts{TProxy: 12345, Socks: 12346}, 1, 0, []int{12346}, false},
		{"socks changed during settle", "settle", service.InboundPorts{TProxy: 12345, Socks: 22346}, service.InboundPorts{TProxy: 12345, Socks: 12346}, 1, 1, []int{12346}, false},
		{"tproxy changed during settle", "settle", service.InboundPorts{TProxy: 22345, Socks: 12346}, service.InboundPorts{TProxy: 12345, Socks: 12346}, 1, 1, []int{12346}, false},
		{"socks changed during probe", "probe", service.InboundPorts{TProxy: 12345, Socks: 22346}, service.InboundPorts{TProxy: 12345, Socks: 12346}, 1, 1, []int{12346, 12346}, false},
		{"tproxy changed during probe", "probe", service.InboundPorts{TProxy: 22345, Socks: 12346}, service.InboundPorts{TProxy: 12345, Socks: 12346}, 1, 1, []int{12346, 12346}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PATH", t.TempDir())
			s, store, xray, output := newFastPersistedFixture(t)
			template := `{"inbounds":[{"tag":"tproxy-in","protocol":"dokodemo-door","port":12345},{"tag":"socks-in","protocol":"socks","port":12346}],"outbounds":[]}`
			if err := os.WriteFile(filepath.Join(filepath.Dir(output), "template.json"), []byte(template), 0600); err != nil {
				t.Fatal(err)
			}
			setPorts := func(ports service.InboundPorts) error {
				return store.UpdateVPNConfig(func(cfg *vpnconfig.VPNDirectorConfig) error {
					if cfg.Advanced == nil {
						cfg.Advanced = make(map[string]interface{})
					}
					cfg.Advanced["xray"] = map[string]interface{}{"tproxy_port": float64(ports.TProxy), "socks_port": float64(ports.Socks)}
					return nil
				})
			}
			if err := setPorts(service.InboundPorts{TProxy: 12345, Socks: 12346}); err != nil {
				t.Fatal(err)
			}
			var changes atomic.Int32
			changePorts := func() error {
				if err := setPorts(tc.changed); err != nil {
					return err
				}
				changes.Add(1)
				return nil
			}
			mustChange := func() {
				if err := changePorts(); err != nil {
					t.Fatal(err)
				}
			}
			if tc.at == "checks" {
				var once sync.Once
				var changeErr error
				s.h.check = func(_ context.Context, keys []string) (monitor.Evidence, error) {
					once.Do(func() { changeErr = changePorts() })
					return copyFastEvidence(s.h.fresh, keys), changeErr
				}
			}
			s.w.Generate = func(server vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
				s.generateCalls++
				cfg, err := store.LoadVPNConfig()
				if err != nil {
					return false, 0, err
				}
				ports := service.InboundPorts{}
				ports.TProxy, ports.Socks = vpnconfig.XrayInboundPorts(cfg)
				generated, seq, err := service.GenerateAndRecordGuardedWalkedServer(store, xray, endpoint.ServerForDial(server), server, ports, guard)
				if generated {
					s.written = append(s.written, server)
					if tc.at == "generate" {
						mustChange()
					}
				}
				return generated, seq, err
			}
			var running service.InboundPorts
			s.w.RestartXray = func() error {
				s.restarts++
				running = fastReadInboundPorts(t, output)
				if tc.at == "restart" {
					mustChange()
				}
				return nil
			}
			settles := 0
			s.w.AfterRestart = func(delay time.Duration) {
				settles++
				if delay != 3*time.Second {
					t.Errorf("settle %v, want three seconds before the main probe", delay)
				}
				if tc.at == "settle" {
					mustChange()
				}
			}
			var probes []int
			s.w.Probe = func(ctx context.Context, port int) error {
				s.probes++
				probes = append(probes, port)
				if err := ctx.Err(); err != nil {
					t.Errorf("unexpected main probe cancellation: %v", err)
					return err
				}
				if s.probes == 1 {
					return errProbe
				}
				if tc.at == "probe" {
					mustChange()
					return errProbe
				}
				if port != running.Socks {
					return errProbe
				}
				return nil
			}
			s.w.Tick(context.Background())
			cfg, err := store.LoadVPNConfig()
			if err != nil {
				t.Fatal(err)
			}
			if changes.Load() != 1 {
				t.Errorf("port mutations %d, want one advanced-only write without manual reselection", changes.Load())
			}
			if !reflect.DeepEqual(probes, tc.probes) || s.restarts != tc.restarts || settles != tc.settles {
				t.Errorf("probed ports %v, restart %d, settle %d; want %v, %d, %d for the guarded generation snapshot", probes, s.restarts, settles, tc.probes, tc.restarts, tc.settles)
			}
			if s.generateCalls != 1 || len(s.written) != 1 || s.f.applies != 0 {
				t.Errorf("generate %d, written %d, apply %d; a port change must not cause another candidate or immediate tunnel fallback", s.generateCalls, len(s.written), s.f.applies)
			}
			if got := fastReadInboundPorts(t, output); got != tc.generated {
				t.Errorf("live generated ports %+v, want %+v", got, tc.generated)
			}
			tp, socks := vpnconfig.XrayInboundPorts(cfg)
			if tp != tc.changed.TProxy || socks != tc.changed.Socks {
				t.Errorf("persisted ports %d/%d, want %d/%d; automatic transition overwrote the settings change", tp, socks, tc.changed.TProxy, tc.changed.Socks)
			}
			assertFastAssignments(t, cfg)
			wantActive := &vpnconfig.ActiveServer{Subscription: "a1b2c3d4", Name: "Backup", Address: "backup.example", Port: 443, Seq: 8}
			wantPreferred := &vpnconfig.ActiveServer{Subscription: "a1b2c3d4", Name: "Oslo", Address: "oslo.example", Port: 443}
			if !reflect.DeepEqual(cfg.Xray.ActiveServer, wantActive) || !reflect.DeepEqual(cfg.Xray.PreferredServer, wantPreferred) {
				t.Errorf("active %+v, preferred %+v; preserve the own record and original choice", cfg.Xray.ActiveServer, cfg.Xray.PreferredServer)
			}
			if tc.switched {
				if len(s.f.notes) != 1 || !strings.Contains(s.f.notes[0], "Backup") || strings.Contains(s.f.notes[0], "back on Xray") {
					t.Errorf("notes %v; want one server-change event without a client restoration", s.f.notes)
				}
			} else if len(s.f.notes) != 0 {
				t.Errorf("notes %v; a superseded port snapshot must not announce success or fallback", s.f.notes)
			}
		})
	}
}

func fastReadInboundPorts(t *testing.T, path string) service.InboundPorts {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Inbounds []struct {
			Tag  string `json:"tag"`
			Port int    `json:"port"`
		} `json:"inbounds"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	ports := service.InboundPorts{}
	for _, inbound := range config.Inbounds {
		switch inbound.Tag {
		case "tproxy-in":
			ports.TProxy = inbound.Port
		case "socks-in":
			ports.Socks = inbound.Port
		}
	}
	return ports
}
