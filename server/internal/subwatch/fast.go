package subwatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync"
	"time"

	"github.com/zinin/vpn-director/server/internal/endpoint"
	"github.com/zinin/vpn-director/server/internal/monitor"
	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchcompat"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

const (
	FastCandidates   = 3
	FastCheckTimeout = 30 * time.Second
)

type HealthMonitor interface {
	Evidence() monitor.Evidence
	CheckEvidence(context.Context, []string) (monitor.Evidence, error)
	ValidateEvidence(monitor.Evidence, []string) error
}

type fastOutcome uint8

const (
	fastInconclusive fastOutcome = iota
	fastSwitched
	fastFallback
	fastCanceled
)

type fastAttempt struct {
	Config  *vpnconfig.VPNDirectorConfig
	Outcome fastOutcome
	Guard   func(*vpnconfig.VPNDirectorConfig) error
	// Err is what ended an attempt that did not switch; nil when the fast
	// path did not apply.
	Err error
}

var errFastEvidence = errors.New("fresh monitor evidence is no longer applicable")

type fastChoice struct {
	server   vpnconfig.Server
	key      string
	evidence monitor.Evidence
}

type fastSelection struct {
	expected       vpnconfig.ActiveServer
	active         vpnconfig.Server
	activeKeys     []string
	links          map[string]string
	ports          *service.InboundPorts
	evidence       monitor.Evidence
	proofRequired  bool
	refreshed      bool
	fallbackProofs []fastChoice
	last           *fastChoice
	recorded       *vpnconfig.Server
}

func copyFastServer(s vpnconfig.Server) vpnconfig.Server {
	s.IPs = append([]string(nil), s.IPs...)
	s.ALPN = append([]string(nil), s.ALPN...)
	s.Outbound = append([]byte(nil), s.Outbound...)
	return s
}

func exactFastServer(subs []vpnconfig.Subscription, a *vpnconfig.ActiveServer) (vpnconfig.Server, bool) {
	if a == nil || a.Name == "" || a.Address == "" || a.Port == 0 {
		return vpnconfig.Server{}, false
	}
	for _, s := range vpnconfig.AllServers(subs) {
		if s.Subscription == a.Subscription && s.Name == a.Name && s.Address == a.Address && s.Port == a.Port {
			return copyFastServer(s), true
		}
	}
	return vpnconfig.Server{}, false
}

func fastCandidatePresent(subs []vpnconfig.Subscription, s vpnconfig.Server) bool {
	key := endpoint.Key(s)
	for _, current := range vpnconfig.AllServers(subs) {
		if serverID(current) != serverID(s) {
			continue
		}
		for _, c := range endpoint.PerAddress([]vpnconfig.Server{current}) {
			if endpoint.Key(c) == key {
				return true
			}
		}
	}
	return false
}

func fastEvidenceStatus(e monitor.Evidence, keys []string, status watchdapi.Status) bool {
	if e.State != watchdapi.StateOK || len(keys) == 0 {
		return false
	}
	for _, key := range keys {
		st, ok := e.Endpoints[key]
		if !ok || st.Status != status {
			return false
		}
	}
	return true
}

func (f *fastSelection) ownership(w *Watch, ctx context.Context, cfg *vpnconfig.VPNDirectorConfig) error {
	if err := w.mutationAllowedContext(ctx); err != nil {
		return err
	}
	if cfg == nil || cfg.Xray.ActiveServer == nil || *cfg.Xray.ActiveServer != f.expected {
		return errSuperseded
	}
	if f.ports != nil {
		tproxy, socks := vpnconfig.XrayInboundPorts(cfg)
		if tproxy != f.ports.TProxy || socks != f.ports.Socks {
			return errFastEvidence
		}
	}
	subs, err := w.loadSubscriptions()
	if err != nil {
		return errFastEvidence
	}
	ids := []string{f.active.Subscription, f.expected.Subscription}
	if f.last != nil {
		ids = append(ids, f.last.server.Subscription)
	}
	for _, c := range f.fallbackProofs {
		ids = append(ids, c.server.Subscription)
		if !fastCandidatePresent(subs, c.server) {
			return errSuperseded
		}
	}
	for _, id := range ids {
		i := vpnconfig.FindSubscription(subs, id)
		link, known := f.links[id]
		if i < 0 || !known || subs[i].URL != link {
			return vpnconfig.ErrSubscriptionGone
		}
	}
	if !f.refreshed {
		current, ok := exactFastServer(subs, vpnconfig.NewActiveServer(f.active))
		if !ok || !reflect.DeepEqual(current, f.active) {
			return errSuperseded
		}
	}
	if f.recorded != nil && !fastCandidatePresent(subs, *f.recorded) {
		return errSuperseded
	}
	if f.last != nil && !fastCandidatePresent(subs, f.last.server) {
		return errSuperseded
	}
	return w.mutationAllowedContext(ctx)
}

func (f *fastSelection) guard(w *Watch, ctx context.Context, cfg *vpnconfig.VPNDirectorConfig) error {
	if err := f.ownership(w, ctx, cfg); err != nil {
		return err
	}
	if f.proofRequired {
		if !fastEvidenceStatus(f.evidence, f.activeKeys, watchdapi.StatusDead) || w.Health.ValidateEvidence(f.evidence, f.activeKeys) != nil {
			return errFastEvidence
		}
		if f.last != nil && w.Health.ValidateEvidence(f.last.evidence, []string{f.last.key}) != nil {
			return errFastEvidence
		}
		for _, c := range f.fallbackProofs {
			if !fastEvidenceStatus(c.evidence, []string{c.key}, watchdapi.StatusDead) || w.Health.ValidateEvidence(c.evidence, []string{c.key}) != nil {
				return errFastEvidence
			}
		}
	}
	return w.mutationAllowedContext(ctx)
}

func (f *fastSelection) choiceGuard(w *Watch, ctx context.Context, c fastChoice) func(*vpnconfig.VPNDirectorConfig) error {
	return func(cfg *vpnconfig.VPNDirectorConfig) error {
		if err := f.guard(w, ctx, cfg); err != nil {
			return err
		}
		if cfg.Xray.Failover != nil || cfg.Xray.PendingRestore != nil {
			return errSuperseded
		}
		subs, err := w.loadSubscriptions()
		if err != nil {
			return errFastEvidence
		}
		i := vpnconfig.FindSubscription(subs, c.server.Subscription)
		if i < 0 || subs[i].URL != f.links[c.server.Subscription] {
			return vpnconfig.ErrSubscriptionGone
		}
		if !fastCandidatePresent(subs, c.server) {
			return errSuperseded
		}
		if !fastEvidenceStatus(c.evidence, []string{c.key}, watchdapi.StatusAlive) || w.Health.ValidateEvidence(c.evidence, []string{c.key}) != nil {
			return errFastEvidence
		}
		return w.mutationAllowedContext(ctx)
	}
}

// Only the transaction's returned seq belongs to this operation.
func (f *fastSelection) record(s vpnconfig.Server, seq int, err error) {
	if err != nil {
		return
	}
	f.expected = *vpnconfig.NewActiveServer(s)
	f.expected.Seq = seq
	copy := copyFastServer(s)
	f.recorded = &copy
}

// BoundGenerationPorts supplies the fast snapshot to the Tick's Generate callback.
// A bound attempt is read only with Tick ownership held.
func (w *Watch) BoundGenerationPorts() (service.InboundPorts, bool) {
	if w.fastOwned == nil || w.fastOwned.ports == nil {
		return service.InboundPorts{}, false
	}
	return *w.fastOwned.ports, true
}

func (w *Watch) fastGuardNow(ctx context.Context, guard func(*vpnconfig.VPNDirectorConfig) error) (*vpnconfig.VPNDirectorConfig, error) {
	if err := w.mutationAllowedContext(ctx); err != nil {
		return nil, err
	}
	if w.LoadVPN == nil {
		return nil, errFastEvidence
	}
	cfg, err := w.LoadVPN()
	if refused := w.mutationAllowedContext(ctx); refused != nil {
		return cfg, refused
	}
	if err != nil {
		return cfg, errFastEvidence
	}
	return cfg, guard(cfg)
}

func fastTerminal(err error) bool {
	return errors.Is(err, errStopped) || errors.Is(err, watchcompat.ErrIncompatible) ||
		errors.Is(err, errSuperseded) || errors.Is(err, vpnconfig.ErrSubscriptionGone)
}

func fastInfrastructure(err error) bool {
	var pathError *os.PathError
	return errors.Is(err, service.ErrConfigLoad) || errors.Is(err, service.ErrConfigLockTimeout) ||
		errors.Is(err, errFastEvidence) || errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) || errors.As(err, &pathError)
}

func (w *Watch) fastFailover(ctx context.Context, cfg *vpnconfig.VPNDirectorConfig) fastAttempt {
	out := fastAttempt{Config: cfg}
	if w.Health == nil || w.WANUp == nil || w.LoadVPN == nil || cfg == nil || cfg.Xray.ActiveServer == nil ||
		cfg.Xray.Failover != nil || cfg.Xray.PendingRestore != nil ||
		(cfg.Monitor != nil && cfg.Monitor.Enabled != nil && !*cfg.Monitor.Enabled) {
		return out
	}
	subs, err := w.loadSubscriptions()
	if err != nil {
		return out
	}
	active, ok := exactFastServer(subs, cfg.Xray.ActiveServer)
	if !ok {
		return out
	}
	keys, seen := []string{}, map[string]bool{}
	for _, key := range endpoint.Keys(active) {
		if !seen[key] {
			keys, seen[key] = append(keys, key), true
		}
	}
	if len(keys) == 0 {
		return out
	}
	cached := w.Health.Evidence()
	if cached.State != watchdapi.StateOK {
		return out
	}
	chosen := cfg.Xray.PreferredServer
	if chosen == nil {
		chosen = cfg.Xray.ActiveServer
	}
	order, _ := walkOrder(subs, chosen)
	var choices []fastChoice
	for _, s := range endpoint.PerAddress(order) {
		key := endpoint.Key(s)
		if seen[key] {
			continue
		}
		seen[key] = true
		if !fastEvidenceStatus(cached, []string{key}, watchdapi.StatusAlive) {
			continue
		}
		if w.Health.ValidateEvidence(cached, []string{key}) != nil {
			return out
		}
		choices = append(choices, fastChoice{server: copyFastServer(s), key: key})
		if len(choices) == FastCandidates {
			break
		}
	}
	owner := &fastSelection{
		expected: *cfg.Xray.ActiveServer, active: active, activeKeys: keys,
		links: make(map[string]string),
	}
	for _, sub := range subs {
		owner.links[sub.ID] = sub.URL
	}
	w.fastOwned = owner
	guard := func(current *vpnconfig.VPNDirectorConfig) error { return owner.guard(w, ctx, current) }
	finish := func(err error) fastAttempt {
		if current, loadErr := w.LoadVPN(); loadErr == nil {
			out.Config = current
			if refused := owner.ownership(w, ctx, current); refused != nil {
				err = refused
			}
		}
		out.Err = err
		if w.mutationEnded(ctx) || fastTerminal(err) {
			if mutationInterrupted(err) {
				w.mutationFailed.Store(true)
				w.setStatus(err)
			}
			out.Outcome = fastCanceled
		} else {
			out.Outcome = fastInconclusive
			out.Guard = func(current *vpnconfig.VPNDirectorConfig) error { return owner.ownership(w, ctx, current) }
		}
		return out
	}
	w.setStatusAction("confirm")
	checkCtx, cancel := context.WithTimeout(ctx, FastCheckTimeout)
	var wg sync.WaitGroup
	var activeEvidence monitor.Evidence
	var activeError error
	var wan bool
	choiceErrors := make([]error, len(choices))
	wg.Add(2 + len(choices))
	go func() {
		defer wg.Done()
		activeEvidence, activeError = w.Health.CheckEvidence(checkCtx, keys)
	}()
	go func() {
		defer wg.Done()
		wan = w.WANUp(checkCtx)
	}()
	for i := range choices {
		go func() {
			defer wg.Done()
			choices[i].evidence, choiceErrors[i] = w.Health.CheckEvidence(checkCtx, []string{choices[i].key})
		}()
	}
	wg.Wait()
	cancel()
	if w.mutationEnded(ctx) {
		out.Outcome = fastCanceled
		return out
	}
	if activeError != nil || !wan || !fastEvidenceStatus(activeEvidence, keys, watchdapi.StatusDead) ||
		w.Health.ValidateEvidence(activeEvidence, keys) != nil {
		return finish(errFastEvidence)
	}
	owner.evidence, owner.proofRequired = activeEvidence, true
	if _, err := w.fastGuardNow(ctx, guard); err != nil {
		return finish(err)
	}
	var usable []fastChoice
	incomplete := false
	for i, c := range choices {
		if choiceErrors[i] != nil || w.Health.ValidateEvidence(c.evidence, []string{c.key}) != nil {
			incomplete = true
			continue
		}
		switch {
		case fastEvidenceStatus(c.evidence, []string{c.key}, watchdapi.StatusAlive):
			usable = append(usable, c)
		case fastEvidenceStatus(c.evidence, []string{c.key}, watchdapi.StatusDead):
		default:
			incomplete = true
		}
	}
	if len(usable) == 0 && incomplete {
		return finish(errFastEvidence)
	}
	if len(usable) == 0 {
		owner.fallbackProofs = append([]fastChoice(nil), choices...)
	}
	for _, c := range usable {
		if w.Generate == nil {
			return finish(errFastEvidence)
		}
		candidateGuard := owner.choiceGuard(w, ctx, c)
		current, err := w.fastGuardNow(ctx, candidateGuard)
		if err != nil {
			return finish(err)
		}
		if owner.ports == nil {
			ports := service.InboundPorts{}
			ports.TProxy, ports.Socks = vpnconfig.XrayInboundPorts(current)
			owner.ports = &ports
		}
		w.setStatusAction("switch")
		generated, seq, err := w.Generate(c.server, candidateGuard)
		if w.mutationEnded(ctx) || fastTerminal(err) || errors.Is(err, errFastEvidence) {
			return finish(err)
		}
		if !generated {
			if fastInfrastructure(err) {
				return finish(err)
			}
			continue
		}
		owner.last = &c
		owner.record(c.server, seq, err)
		// The candidate's config is published: ownership alone - a stop,
		// incompatibility, a newer selection, a subscription or server gone -
		// ends the switch now. Evidence a newer check replaced would leave the
		// config on the candidate and Xray on the dead server.
		published := func(current *vpnconfig.VPNDirectorConfig) error { return owner.ownership(w, ctx, current) }
		if _, err := w.fastGuardNow(ctx, published); err != nil {
			return finish(err)
		}
		restartErr := w.restartXray()
		if _, err := w.fastGuardNow(ctx, published); err != nil {
			return finish(err)
		}
		if restartErr != nil {
			if fastTerminal(restartErr) || fastInfrastructure(restartErr) {
				return finish(restartErr)
			}
			continue
		}
		w.AfterRestart(SettleAfterRestart)
		current, err = w.fastGuardNow(ctx, published)
		if err != nil {
			return finish(err)
		}
		probeErr := w.Probe(ctx, w.socksPort(current))
		current, err = w.fastGuardNow(ctx, published)
		if err != nil {
			return finish(err)
		}
		if probeErr != nil {
			if fastTerminal(probeErr) {
				return finish(probeErr)
			}
			continue
		}
		if !w.returnDeath.Equal(w.failSince) {
			w.returnDeath = w.failSince
			w.returnAfterDeath()
		}
		picked := copyFastServer(c.server)
		w.lastPicked = &picked
		w.settled()
		w.lastRouteKind, w.lastImportKind = noteNone, noteNone
		w.setStatusConfig(current)
		w.notify(noteRestored, fmt.Sprintf("Xray switched to server %s", label(subscriptionNames(subs), c.server.Subscription, c.server.Name)))
		return fastAttempt{Config: current, Outcome: fastSwitched}
	}
	current, err := w.fastGuardNow(ctx, guard)
	if err != nil {
		return finish(err)
	}
	return fastAttempt{Config: current, Outcome: fastFallback, Guard: guard}
}

// A scoped fallback carries ownership through its applies, refresh and walk.
func (w *Watch) outboundAllowedNow() error {
	if w.outboundGuard == nil {
		return nil
	}
	if w.outboundRefused != nil {
		return w.outboundRefused
	}
	_, err := w.fastGuardNow(w.mutationContext, w.outboundGuard)
	if err != nil {
		w.outboundRefused = err
	}
	return err
}

func (w *Watch) generateWalked(s vpnconfig.Server, guard func(*vpnconfig.VPNDirectorConfig) error) (bool, int, error) {
	if w.outboundGuard == nil {
		return w.Generate(s, guard)
	}
	if err := w.outboundAllowedNow(); err != nil {
		return false, 0, err
	}
	combined := func(cfg *vpnconfig.VPNDirectorConfig) error {
		if err := w.outboundGuard(cfg); err != nil {
			w.outboundRefused = err
			return err
		}
		if guard != nil {
			if err := guard(cfg); err != nil {
				return err
			}
		}
		subs, err := w.loadSubscriptions()
		if err != nil {
			return errFastEvidence
		}
		if !fastCandidatePresent(subs, s) {
			return vpnconfig.ErrSubscriptionGone
		}
		if w.fastOwned != nil {
			if _, known := w.fastOwned.links[s.Subscription]; !known {
				i := vpnconfig.FindSubscription(subs, s.Subscription)
				w.fastOwned.links[s.Subscription] = subs[i].URL
			}
		}
		return w.mutationAllowed()
	}
	generated, seq, err := w.Generate(s, combined)
	if generated && w.fastOwned != nil {
		w.fastOwned.record(s, seq, err)
	}
	return generated, seq, err
}
