package monitor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

// Cadences of the monitor's loop.
const (
	// RefreshEvery is how often the monitor rereads its settings and the
	// subscriptions and looks at the state of VPN Director.
	RefreshEvery = time.Minute
	// SaveEvery is how often a changed state is saved.
	SaveEvery = time.Minute
	// ControlEvery is how often the control addresses are dialed while the
	// WAN is down.
	ControlEvery = 15 * time.Second
	// GuardWindow and GuardMin: when at least GuardMin checks, or every
	// endpoint when there are fewer, completed within GuardWindow and all of
	// them failed, the monitor asks whether the WAN is up.
	GuardWindow = 30 * time.Second
	GuardMin    = 5
	// MaxRefusals bounds the endpoints one start of the prober may reject.
	MaxRefusals = 20
	// CrashLimit crashes within CrashWindow stop the restarts for a backoff.
	CrashLimit  = 5
	CrashWindow = 10 * time.Minute
)

// retryAfter is the wait before a failed attempt's retry, crashGrace the time
// a lone suspect's prober gets to crash after its check, and proberBackoff the
// waits after failed starts. Vars so a test can shorten them.
var (
	retryAfter    = 2 * time.Second
	crashGrace    = time.Second
	proberBackoff = []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute}
)

// Deps is what a monitor runs on. Settings, Endpoints, Launcher, Stopped and
// WANUp are required.
type Deps struct {
	// Settings reads the monitor section, resolved.
	Settings func() (Settings, error)
	// Endpoints builds the endpoint set (Build) from the subscriptions.
	Endpoints func() ([]Endpoint, map[string]string, error)
	Launcher  Launcher
	// Stopped reports VPN Director stopped (/tmp/vpn-director/stopped).
	Stopped func() bool
	// WANUp reports whether a control address accepts.
	WANUp func(ctx context.Context) bool
	// StatePath is where the state survives a restart; empty keeps none.
	StatePath string
	// OnSettings sees every resolved Settings, e.g. for the log level.
	OnSettings func(Settings)
	// Now and Jitter default to time.Now and a uniform value in [-1, 1).
	Now    func() time.Time
	Jitter func() float64
}

// Monitor checks every endpoint of every subscription through a prober. Run
// drives it; its public methods are safe to call from other goroutines.
// Only Run's goroutine changes the session and the entries' set.
type Monitor struct {
	d Deps

	mu           sync.Mutex
	settings     Settings
	state        watchdapi.State
	message      string
	entries      map[string]*entry
	order        []string       // the endpoints' keys in Build order
	pos          map[string]int // each key's place in order: the active server's first
	session      Session
	setKey       string                // the keyed outbounds the session holds
	restored     map[string]savedEntry // state read at startup, until the first refresh takes it
	sequence     uint64                // completed checks, including unpublished failures
	starts       uint64                // dispatch order, including checks still in flight
	generation   uint64                // endpoint-set, session and activity epoch
	closed       bool                  // Run's lifetime has ended
	pending      map[string]outcome    // failures awaiting WAN evidence
	recent       []outcome             // completed checks within GuardWindow
	controlOK    time.Time             // when a control last answered
	nextControl  time.Time             // the next control dial while the WAN is down
	proberFails  int                   // failed starts in a row
	proberAt     time.Time             // no start before this
	proberError  string                // retry reason, also retained during a WAN pause
	crashBatches int                   // crash-limit waits since a stable main session
	sessionSince time.Time             // start of the current uninterrupted main session
	wanPaused    bool                  // only a successful control releases this guard
	suspects     []string              // deferred isolation, bounded by MaxConcurrency
	crashes      []time.Time
	lag          time.Duration
	lagWarned    bool
	dirty        bool
	lastSave     time.Time
	updated      time.Time
	warned       map[string]string
	changed      chan struct{} // closed and replaced whenever a result lands

	results chan result
	wake    chan struct{}
}

// result is a worker's answer for one endpoint.
type result struct {
	sess       Session
	key        string
	latency    time.Duration
	err        error
	seq        uint64
	at         time.Time
	generation uint64
	started    uint64
}

// outcome is a completed check; failures stay private until WAN evidence.
type outcome struct {
	at         time.Time
	ok         bool
	key        string
	reason     string
	seq        uint64
	generation uint64
	started    uint64
}

// New returns a monitor over d; Run starts it.
func New(d Deps) *Monitor {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Jitter == nil {
		d.Jitter = func() float64 { return rand.Float64()*2 - 1 }
	}
	s, _ := SettingsFrom(nil)
	return &Monitor{
		d:          d,
		settings:   s,
		state:      watchdapi.StateOK,
		generation: 1,
		entries:    map[string]*entry{},
		pending:    map[string]outcome{},
		warned:     map[string]string{},
		changed:    make(chan struct{}),
		results:    make(chan result, MaxConcurrency),
		wake:       make(chan struct{}, 1),
	}
}

// Run checks until ctx ends, then stops the prober and saves the state.
func (m *Monitor) Run(ctx context.Context) {
	m.restore()
	var nextRefresh time.Time
	for {
		now := m.d.Now()
		if !now.Before(nextRefresh) {
			m.refresh(ctx, now)
			nextRefresh = now.Add(RefreshEvery)
		}
		m.tick(ctx, now)
		var exited <-chan struct{}
		if m.session != nil {
			exited = m.session.Exited()
		}
		timer := time.NewTimer(m.untilNext(now, nextRefresh))
		select {
		case <-ctx.Done():
			timer.Stop()
			m.shutdown()
			return
		case r := <-m.results:
			m.apply(ctx, r)
		case <-m.wake:
		case <-exited:
			m.crashed(ctx)
		case <-timer.C:
		}
		timer.Stop()
	}
}

// Snapshot is the monitor's state for GET /v1/monitor.
func (m *Monitor) Snapshot() watchdapi.Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	eps := make(map[string]watchdapi.EndpointState, len(m.entries))
	for k, e := range m.entries {
		eps[k] = e.st
	}
	return watchdapi.Snapshot{
		State:           m.state,
		Message:         m.message,
		IntervalSeconds: int(m.settings.Interval / time.Second),
		LagSeconds:      int(m.lag / time.Second),
		UpdatedAt:       m.updated,
		Endpoints:       eps,
	}
}

// Request makes the endpoints of keys - every endpoint when keys is empty -
// due ahead of the rest and answers how many it queued; an endpoint already
// under check counts, since its answer is coming. Unknown keys are ignored.
func (m *Monitor) Request(keys []string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.request(keys)
}

// request queues checks with mu held.
func (m *Monitor) request(keys []string) (int, error) {
	return m.queue(keys, false)
}

// queue registers Request or Check intent with mu held.
func (m *Monitor) queue(keys []string, fresh bool) (int, error) {
	if m.state == watchdapi.StateStopped || m.state == watchdapi.StateDisabled {
		return 0, watchdapi.ErrNotActive
	}
	n := 0
	mark := func(e *entry) {
		if !e.checkable() {
			return
		}
		if fresh {
			e.followUp, e.after = true, m.sequence
		} else if !e.inFlight {
			e.urgent = true
		}
		n++
	}
	if len(keys) == 0 {
		for _, e := range m.entries {
			mark(e)
		}
	} else {
		for _, k := range keys {
			if e := m.entries[k]; e != nil {
				mark(e)
			}
		}
	}
	select {
	case m.wake <- struct{}{}:
	default:
	}
	return n, nil
}

// Check makes the endpoints of keys due now and waits until each has an
// answer from after the call, or ctx ends; it returns their states as they
// stand then. A key not in the set yet is waited for too. The watch's
// failover (stage 3) calls it in-process, with a deadline.
func (m *Monitor) Check(ctx context.Context, keys []string) (map[string]watchdapi.EndpointState, error) {
	m.mu.Lock()
	start := m.sequence
	if len(keys) == 0 {
		keys = make([]string, 0, len(m.entries))
		for k := range m.entries {
			keys = append(keys, k)
		}
	}
	_, err := m.queue(keys, true)
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	for {
		m.mu.Lock()
		out := make(map[string]watchdapi.EndpointState, len(keys))
		done := true
		for _, k := range keys {
			e := m.entries[k]
			if e == nil {
				done = false
				continue
			}
			out[k] = e.st
			if e.checkable() && (e.completed <= start || e.completedSession != m.session || m.session == nil || exited(m.session)) {
				done = false
			}
		}
		inactive := m.state == watchdapi.StateStopped || m.state == watchdapi.StateDisabled
		ch := m.changed
		m.mu.Unlock()
		if inactive {
			return out, watchdapi.ErrNotActive
		}
		if done {
			return out, nil
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-ch:
		}
	}
}

// refresh rereads the settings and the subscriptions, looks at the state of
// VPN Director and starts, keeps or stops the prober.
func (m *Monitor) refresh(ctx context.Context, now time.Time) {
	s, err := m.d.Settings()
	if err != nil {
		m.warnOnce("settings", "Monitor cannot read its settings; keeping the last ones", err)
		s = m.settingsNow()
	} else {
		m.warnOnce("settings", "", nil)
	}
	m.mu.Lock()
	m.settings = s
	m.mu.Unlock()
	if m.d.OnSettings != nil {
		m.d.OnSettings(s)
	}
	eps, refused, err := m.d.Endpoints()
	if err != nil {
		m.warnOnce("endpoints", "Monitor cannot read the subscriptions; keeping the last list", err)
	} else {
		m.warnOnce("endpoints", "", nil)
		m.merge(now, eps, refused)
	}
	switch {
	case !s.Enabled:
		m.idle(watchdapi.StateDisabled, "")
		return
	case m.d.Stopped():
		m.idle(watchdapi.StateStopped, "")
		return
	}
	if m.wanPaused {
		if ctx.Err() != nil {
			return
		}
		// Re-detect Xray without clearing the guard or its crash retry deadline.
		err := m.d.Launcher.Ready()
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, ErrNoXray) {
			m.idle(watchdapi.StateNoXray, err.Error())
			return
		}
		m.setState(watchdapi.StateWANDown, "")
		return
	}
	m.resume(ctx, now)
}

// resume retries the prober and any deferred isolation after WAN recovery.
func (m *Monitor) resume(ctx context.Context, now time.Time) {
	if ctx.Err() != nil || now.Before(m.proberAt) {
		return
	}
	if err := m.d.Launcher.Ready(); err != nil {
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, ErrNoXray) {
			m.idle(watchdapi.StateNoXray, err.Error())
			m.proberAt, m.proberFails = time.Time{}, 0
		} else {
			m.stopSession()
			m.startFailed(now, err)
		}
		return
	}
	if m.isolate(ctx, now) {
		m.ensureSession(ctx, now)
	}
}

func (m *Monitor) settingsNow() Settings {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.settings
}

// merge takes a new endpoint set: new endpoints are due at once - or as the
// saved state has them - gone ones leave, and the generator's refusals are
// decided again.
func (m *Monitor) merge(now time.Time, eps []Endpoint, refused map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	changed := len(m.restored) > 0
	identityChanged := false
	next := make(map[string]*entry, len(eps)+len(refused))
	order := make([]string, 0, len(eps))
	for _, ep := range eps {
		e := m.entries[ep.Key]
		if e == nil {
			e = m.fromRestored(ep.Key, now)
			identityChanged = true
		}
		if !bytes.Equal(e.ep.Outbound, ep.Outbound) {
			identityChanged = true
			e.rejectedCurrent = false
		}
		e.ep = ep
		if e.st.Status == watchdapi.StatusRejected && !e.sticky {
			// The generator refused it before and does no longer.
			e.st = watchdapi.EndpointState{Status: watchdapi.StatusUnknown, NextAt: now, Since: now}
			e.revision++
			e.rejectedCurrent = false
			identityChanged, changed = true, true
		}
		next[ep.Key] = e
		order = append(order, ep.Key)
	}
	for key, reason := range refused {
		e := m.entries[key]
		if e == nil {
			e = m.fromRestored(key, now)
			identityChanged = true
		}
		if e.checkable() || e.sticky || len(e.ep.Outbound) != 0 {
			identityChanged = true
		}
		e.ep = Endpoint{Key: key}
		before := e.st
		e.reject(now, reason, false)
		changed = changed || before != e.st
		next[key] = e
	}
	identityChanged = identityChanged || len(next) != len(m.entries) || !sameKeys(next, m.entries)
	if changed || identityChanged {
		m.dirty = true
		m.updated = now
	}
	m.entries = next
	m.order = order
	m.pos = make(map[string]int, len(order))
	for i, k := range order {
		m.pos[k] = i
	}
	m.restored = nil
	if identityChanged {
		m.invalidateEvidence()
	} else {
		m.notify()
	}
}

func sameKeys(a, b map[string]*entry) bool {
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

// fromRestored is the entry the saved state has for key, or a new one due now.
func (m *Monitor) fromRestored(key string, now time.Time) *entry {
	if r, ok := m.restored[key]; ok {
		return &entry{st: r.State, pause: r.Pause, sticky: r.Sticky}
	}
	return &entry{st: watchdapi.EndpointState{Status: watchdapi.StatusUnknown, NextAt: now, Since: now}}
}

// checkableSet is the endpoints the prober holds, in Build order.
func (m *Monitor) checkableSet() []Endpoint {
	m.mu.Lock()
	defer m.mu.Unlock()
	var set []Endpoint
	for _, k := range m.order {
		if e := m.entries[k]; e != nil && e.checkable() {
			set = append(set, e.ep)
		}
	}
	return set
}

// keysOf identifies keyed outbounds independently of labels and Build order.
func keysOf(eps []Endpoint) string {
	keys := make([]string, len(eps))
	for i, ep := range eps {
		keys[i] = ep.Key + ":" + fmt.Sprintf("%x", sha256.Sum256(ep.Outbound))
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// ensureSession runs a prober holding the checkable set, unless one holds it
// already. Xray refusing an outbound rejects that endpoint and starts again.
func (m *Monitor) ensureSession(ctx context.Context, now time.Time) {
	if m.wanPaused || ctx.Err() != nil {
		return
	}
	set := m.checkableSet()
	if m.session != nil && keysOf(set) == m.setKey {
		if !exited(m.session) {
			m.setState(watchdapi.StateOK, "")
		}
		return
	}
	if len(set) == 0 {
		m.stopSession()
		m.setState(watchdapi.StateOK, "")
		return
	}
	if now.Before(m.proberAt) {
		return
	}
	m.stopSession()
	for range MaxRefusals {
		if ctx.Err() != nil {
			return
		}
		sess, err := m.d.Launcher.Start(ctx, set)
		if ctx.Err() != nil {
			if sess != nil {
				sess.Stop()
			}
			return
		}
		if err == nil {
			setKey := keysOf(set)
			m.mu.Lock()
			m.session, m.setKey, m.sessionSince = sess, setKey, m.d.Now()
			m.invalidateEvidence()
			m.mu.Unlock()
			m.proberFails, m.proberAt = 0, time.Time{}
			m.setState(watchdapi.StateOK, "")
			return
		}
		if !isConfigRefusal(err) {
			m.startFailed(now, err)
			return
		}
		var refused *RefusedError
		bad, reason := "", ""
		if errors.As(err, &refused) {
			bad, reason = refused.Key, refused.Reason
		} else {
			var testErr error
			bad, testErr = m.bisect(ctx, set)
			if ctx.Err() != nil {
				return
			}
			if testErr != nil {
				m.startFailed(now, testErr)
				return
			}
			reason = "Xray refused the outbound"
		}
		if bad == "" {
			m.startFailed(now, err)
			return
		}
		next := without(set, bad)
		if len(next) == len(set) {
			m.startFailed(now, errors.New("Xray refused an endpoint outside the prober set"))
			return
		}
		m.rejectKey(now, bad, reason)
		set = next
		if len(set) == 0 {
			m.setState(watchdapi.StateOK, "")
			return
		}
	}
	m.startFailed(now, fmt.Errorf("Xray refused %d outbounds in a row", MaxRefusals))
}

// bisect finds an endpoint whose outbound Xray refuses when its error names
// none: halves of set are tested with "xray run -test" down to one endpoint.
// Only typed config refusals narrow the set; infrastructure errors stop it.
func (m *Monitor) bisect(ctx context.Context, set []Endpoint) (string, error) {
	test := func(eps []Endpoint) (bool, error) {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		err := m.d.Launcher.Test(ctx, eps)
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if err == nil {
			return false, nil
		}
		if !isConfigRefusal(err) {
			return false, err
		}
		return true, nil
	}
	bad, err := test(set)
	if err != nil || !bad {
		return "", err
	}
	for len(set) > 1 {
		half := set[:len(set)/2]
		bad, err := test(half)
		if err != nil {
			return "", err
		}
		if bad {
			set = half
		} else {
			set = set[len(set)/2:]
		}
	}
	bad, err = test(set)
	if err != nil || !bad {
		return "", err
	}
	return set[0].Key, nil
}

func isConfigRefusal(err error) bool {
	var refusal interface{ ConfigRefusal() bool }
	return errors.As(err, &refusal) && refusal.ConfigRefusal()
}

func without(set []Endpoint, key string) []Endpoint {
	out := make([]Endpoint, 0, len(set))
	for _, ep := range set {
		if ep.Key != key {
			out = append(out, ep)
		}
	}
	return out
}

func (m *Monitor) rejectKey(now time.Time, key, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[key]
	if e == nil {
		return
	}
	wasCheckable := e.checkable()
	e.reject(now, reason, true)
	m.dirty = true
	m.updated = now
	if wasCheckable {
		m.invalidateEvidence()
	} else {
		m.notify()
	}
	slog.Warn("Monitor: Xray refused a server", "server", e.ep.Label, "reason", reason)
}

// startFailed records a prober that did not start and backs off 1, 2, then 5
// minutes before the next try.
func (m *Monitor) startFailed(now time.Time, err error) {
	wait := proberBackoff[min(m.proberFails, len(proberBackoff)-1)]
	m.proberFails++
	m.proberAt, m.proberError = now.Add(wait), err.Error()
	if !m.wanPaused {
		m.setState(watchdapi.StateProberError, m.proberError)
	}
	slog.Warn("Monitor: the prober did not start", "error", err, "retry_in", wait)
}

// stopSession stops the prober; the answers still on their way are dropped.
func (m *Monitor) stopSession() {
	if m.session == nil {
		return
	}
	sess := m.session
	m.mu.Lock()
	m.session, m.setKey, m.sessionSince = nil, "", time.Time{}
	for _, e := range m.entries {
		e.inFlight = false
	}
	m.discardPending()
	m.recent = nil
	m.invalidateEvidence()
	m.mu.Unlock()
	sess.Stop()
}

// idle stops the checks for state: the statuses stay as they were.
func (m *Monitor) idle(state watchdapi.State, message string) {
	m.stopSession()
	m.mu.Lock()
	m.recent = nil
	m.mu.Unlock()
	m.setState(state, message)
}

func (m *Monitor) setState(state watchdapi.State, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == state && m.message == message {
		return
	}
	stateChanged := m.state != state
	m.state, m.message = state, message
	m.updated = m.d.Now()
	if stateChanged {
		m.invalidateEvidence()
	} else {
		m.notify()
	}
	slog.Info("Monitor state", "state", state, "message", message)
}

// tick dials the controls while the WAN is down, hands due checks to free
// workers and saves a changed state now and then.
func (m *Monitor) tick(ctx context.Context, now time.Time) {
	if ctx.Err() != nil {
		return
	}
	if m.wanPaused && m.state == watchdapi.StateWANDown && !now.Before(m.nextControl) {
		up := m.d.WANUp(ctx)
		if ctx.Err() != nil {
			return
		}
		if up {
			m.mu.Lock()
			for _, e := range m.entries {
				if e.checkable() {
					e.st.NextAt = now
				}
			}
			m.recent = nil
			m.controlOK = now
			m.wanPaused = false
			m.dirty = true
			m.mu.Unlock()
			slog.Info("Monitor: the WAN is back; checking every server")
			if now.Before(m.proberAt) {
				m.setState(watchdapi.StateProberError, m.proberError)
			} else {
				m.resume(ctx, now)
			}
		} else {
			m.nextControl = now.Add(ControlEvery)
		}
	}
	if m.state == watchdapi.StateOK && m.session != nil && !exited(m.session) {
		m.mu.Lock()
		deadline := m.pendingUntil()
		m.mu.Unlock()
		if !deadline.IsZero() && !now.Before(deadline) {
			m.guard(ctx, now)
		}
		if m.state == watchdapi.StateOK && ctx.Err() == nil {
			m.dispatch(ctx, now)
		}
	}
	if m.dirty && now.Sub(m.lastSave) >= SaveEvery {
		m.save(now)
	}
}

// dispatch hands the due endpoints - urgent ones first, then the longest due,
// then in Build order, the active server's first - to free workers, and
// measures how long the next one waits.
func (m *Monitor) dispatch(ctx context.Context, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	busy := 0
	var due []*entry
	for _, e := range m.entries {
		switch {
		case e.inFlight:
			busy++
		case e.checkable() && !e.pending && (e.urgent || e.followUp || e.evidenceFollowUp || !e.st.NextAt.After(now)):
			due = append(due, e)
		}
	}
	sort.Slice(due, func(i, j int) bool {
		iUrgent := due[i].urgent || due[i].followUp || due[i].evidenceFollowUp
		jUrgent := due[j].urgent || due[j].followUp || due[j].evidenceFollowUp
		if iUrgent != jUrgent {
			return iUrgent
		}
		if !due[i].st.NextAt.Equal(due[j].st.NextAt) {
			return due[i].st.NextAt.Before(due[j].st.NextAt)
		}
		return m.pos[due[i].ep.Key] < m.pos[due[j].ep.Key]
	})
	free := max(0, m.settings.Concurrency-busy)
	n := min(free, len(due))
	for _, e := range due[:n] {
		e.inFlight, e.urgent = true, false
		m.starts++
		go m.check(ctx, m.session, e.ep.Key, m.generation, m.starts)
	}
	m.lag = 0
	if waiting := due[n:]; len(waiting) > 0 && !waiting[0].urgent && !waiting[0].followUp && !waiting[0].evidenceFollowUp {
		m.lag = now.Sub(waiting[0].st.NextAt)
	}
	switch {
	case m.lag > m.settings.Interval && !m.lagWarned:
		m.lagWarned = true
		slog.Warn("Monitor: checks are falling behind; raise monitor.concurrency or monitor.interval", "lag", m.lag)
	case m.lag <= m.settings.Interval:
		m.lagWarned = false
	}
}

// check is a worker: one attempt, and after a failure one retry retryAfter
// later - unless the prober went meanwhile, a crash that is not the server's.
func (m *Monitor) check(ctx context.Context, sess Session, key string, generation, started uint64) {
	latency, err := sess.Check(ctx, key)
	if err != nil && ctx.Err() == nil {
		select {
		case <-ctx.Done():
		case <-sess.Exited():
		case <-time.After(retryAfter):
			latency, err = sess.Check(ctx, key)
		}
	}
	r := m.finish(result{sess: sess, key: key, latency: latency, err: err, generation: generation, started: started})
	select {
	case m.results <- r:
	case <-ctx.Done():
	}
}

// finish linearizes terminal completion with Check registration before enqueue.
// Workers allocate only completion metadata; Run owns entries and sessions.
func (m *Monitor) finish(r result) result {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sequence++
	r.seq, r.at = m.sequence, m.d.Now()
	return r
}

func exited(s Session) bool {
	select {
	case <-s.Exited():
		return true
	default:
		return false
	}
}

// apply records a worker's answer. An answer of a prober that is gone, or that
// crashed, or one that comes while the WAN is down counts for nothing.
func (m *Monitor) apply(ctx context.Context, r result) {
	m.mu.Lock()
	e := m.entries[r.key]
	if r.seq == 0 || e == nil || r.sess != m.session || r.sess == nil || exited(r.sess) {
		// A crashed prober's in-flight endpoints remain isolation suspects.
		m.mu.Unlock()
		return
	}
	e.inFlight = false
	if r.generation != m.generation || r.started == 0 || m.state != watchdapi.StateOK || ctx.Err() != nil {
		m.mu.Unlock()
		return
	}
	now := m.d.Now()
	o := outcome{at: now, key: r.key, ok: r.err == nil, seq: r.seq, generation: r.generation, started: r.started}
	m.recent = append(m.recent, o)
	m.pruneRecent(now)
	trip := false
	if r.err == nil {
		if r.at.Sub(m.sessionSince) >= CrashWindow {
			m.crashBatches, m.crashes = 0, nil
		}
		if e.succeed(now, r.latency, m.settings, m.d.Jitter()) {
			slog.Info("Monitor: server alive", "server", e.ep.Label, "latency", r.latency.Round(time.Millisecond))
		}
		m.completed(e, o)
		m.publishPending()
	} else {
		o.reason = classify(r.err)
		e.pending = true
		m.pending[r.key] = o
		if m.wanEvidence(now) {
			m.publishPending()
		} else {
			trip = m.guardDue(now)
		}
	}
	m.mu.Unlock()
	if trip {
		m.guard(ctx, now)
	}
}

// completed publishes freshness only for a resolved completion, with mu held.
func (m *Monitor) completed(e *entry, o outcome) {
	e.completed, e.completedSession = o.seq, m.session
	e.completedGeneration, e.completedStart = o.generation, o.started
	if e.followUp && o.seq > e.after {
		e.followUp = false
	}
	if e.evidenceFollowUp && o.generation == m.generation && o.started > e.evidenceAfter {
		e.evidenceFollowUp = false
	}
	m.dirty = true
	m.updated = m.d.Now()
	m.notify()
}

// notify wakes Check waiters; the caller holds mu.
func (m *Monitor) notify() {
	close(m.changed)
	m.changed = make(chan struct{})
}

// publishPending counts failures only after WAN evidence, with mu held.
func (m *Monitor) publishPending() {
	for key, o := range m.pending {
		if e := m.entries[key]; e != nil && e.checkable() && e.pending {
			if e.fail(o.at, o.reason, m.settings) {
				slog.Info("Monitor: server down", "server", e.ep.Label, "error", o.reason)
			}
			e.pending = false
			m.completed(e, o)
		}
		delete(m.pending, key)
	}
}

// discardPending leaves public statuses and pauses untouched, with mu held.
func (m *Monitor) discardPending() {
	for _, e := range m.entries {
		e.pending = false
	}
	clear(m.pending)
}

// pendingUntil bounds unresolved failures even below GuardMin, with mu held.
func (m *Monitor) pendingUntil() time.Time {
	var next time.Time
	for _, o := range m.pending {
		deadline := o.at.Add(GuardWindow)
		if next.IsZero() || deadline.Before(next) {
			next = deadline
		}
	}
	return next
}

func (m *Monitor) wanEvidence(now time.Time) bool {
	if !m.controlOK.IsZero() && now.Sub(m.controlOK) < GuardWindow {
		return true
	}
	for _, o := range m.recent {
		if o.ok && now.Sub(o.at) < GuardWindow {
			return true
		}
	}
	return false
}

// pruneRecent drops the checks that completed before GuardWindow. The caller
// holds mu.
func (m *Monitor) pruneRecent(now time.Time) {
	kept := m.recent[:0]
	for _, o := range m.recent {
		if now.Sub(o.at) < GuardWindow {
			kept = append(kept, o)
		}
	}
	m.recent = kept
}

// guardDue reports failures enough to ask whether the WAN is up: at least
// GuardMin checks - or every checkable endpoint, when there are fewer -
// completed within GuardWindow, all failed, and no control answered within
// that window. The caller holds mu.
func (m *Monitor) guardDue(now time.Time) bool {
	m.pruneRecent(now)
	checkable := 0
	for _, e := range m.entries {
		if e.checkable() {
			checkable++
		}
	}
	need := min(GuardMin, checkable)
	if need == 0 || len(m.recent) < need || now.Sub(m.controlOK) < GuardWindow {
		return false
	}
	for _, o := range m.recent {
		if o.ok {
			return false
		}
	}
	return true
}

// guard resolves pending failures: a live control publishes them, a dead WAN
// discards them without changing public statuses or pauses.
func (m *Monitor) guard(ctx context.Context, now time.Time) {
	up := m.d.WANUp(ctx)
	if ctx.Err() != nil {
		return
	}
	m.mu.Lock()
	if up {
		m.controlOK = now
		m.publishPending()
		m.mu.Unlock()
		return
	}
	m.discardPending()
	m.recent = nil
	m.nextControl = now.Add(ControlEvery)
	m.wanPaused = true
	m.mu.Unlock()
	m.setState(watchdapi.StateWANDown, "")
	slog.Warn("Monitor: the WAN is down; server statuses are kept until it is back")
}

// crashed handles a prober that exited on its own: each endpoint under check
// at that moment is checked alone in a prober of its own, and one that crashes
// that prober too is rejected. Then the prober starts again without the
// culprits, unless it keeps crashing.
func (m *Monitor) crashed(ctx context.Context) {
	now := m.d.Now()
	m.mu.Lock()
	sess := m.session
	if sess == nil {
		m.mu.Unlock()
		return
	}
	seen := make(map[string]bool, len(m.suspects))
	for _, key := range m.suspects {
		seen[key] = true
	}
	for _, k := range m.order {
		if e := m.entries[k]; e != nil && e.inFlight && !seen[k] && len(m.suspects) < MaxConcurrency {
			m.suspects = append(m.suspects, k)
			seen[k] = true
		}
	}
	for _, e := range m.entries {
		e.inFlight = false
	}
	m.session, m.setKey, m.sessionSince = nil, "", time.Time{}
	m.discardPending()
	m.recent = nil
	m.invalidateEvidence()
	kept := m.crashes[:0]
	for _, t := range m.crashes {
		if now.Sub(t) < CrashWindow {
			kept = append(kept, t)
		}
	}
	m.crashes = append(kept, now)
	crashes := len(m.crashes)
	m.mu.Unlock()
	sess.Stop()
	slog.Warn("Monitor: the prober exited", "suspects", len(m.suspects))
	if !m.wanPaused && !m.isolate(ctx, now) {
		return
	}
	if ctx.Err() != nil {
		return
	}
	if crashes >= CrashLimit {
		wait := proberBackoff[min(m.crashBatches, len(proberBackoff)-1)]
		m.crashBatches++
		m.crashes = nil
		m.proberAt = now.Add(wait)
		m.proberError = fmt.Sprintf("the prober crashed %d times in %s", crashes, CrashWindow)
		if !m.wanPaused {
			m.setState(watchdapi.StateProberError, m.proberError)
		}
		slog.Warn("Monitor: the crash loop is paused", "retry_in", wait)
		return
	}
	m.ensureSession(ctx, now)
}

// isolate consumes deferred suspects only while WAN evidence permits traffic.
func (m *Monitor) isolate(ctx context.Context, now time.Time) bool {
	if m.wanPaused {
		return false
	}
	for len(m.suspects) > 0 {
		if ctx.Err() != nil {
			return false
		}
		key := m.suspects[0]
		m.mu.Lock()
		e := m.entries[key]
		var ep Endpoint
		if e != nil && e.checkable() {
			ep = e.ep
		}
		m.mu.Unlock()
		if ep.Key != "" {
			crashed := m.crashesAlone(ctx, ep)
			if ctx.Err() != nil {
				return false
			}
			if crashed {
				m.rejectKey(now, key, "crashes Xray")
			}
		}
		m.suspects = m.suspects[1:]
	}
	return true
}

// crashesAlone checks ep in a prober holding it alone and reports whether that
// prober exited within crashGrace of the check.
func (m *Monitor) crashesAlone(ctx context.Context, ep Endpoint) bool {
	sess, err := m.d.Launcher.Start(ctx, []Endpoint{ep})
	if err != nil {
		return false
	}
	defer sess.Stop()
	_, _ = sess.Check(ctx, ep.Key)
	select {
	case <-sess.Exited():
		return ctx.Err() == nil
	case <-time.After(crashGrace):
		return false
	case <-ctx.Done():
		return false
	}
}

// untilNext is how long the loop may sleep: until the next refresh, the next
// due check a free worker could take, the next control dial or the next save.
func (m *Monitor) untilNext(now, nextRefresh time.Time) time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := nextRefresh
	if m.state == watchdapi.StateWANDown && m.nextControl.Before(next) {
		next = m.nextControl
	}
	if m.state == watchdapi.StateOK && m.session != nil {
		if deadline := m.pendingUntil(); !deadline.IsZero() && deadline.Before(next) {
			next = deadline
		}
		busy := 0
		for _, e := range m.entries {
			if e.inFlight {
				busy++
			}
		}
		if busy < m.settings.Concurrency {
			for _, e := range m.entries {
				if e.inFlight || e.pending || !e.checkable() {
					continue
				}
				if e.urgent || e.followUp || e.evidenceFollowUp {
					return 0
				}
				if e.st.NextAt.Before(next) {
					next = e.st.NextAt
				}
			}
		}
	}
	if m.dirty && m.lastSave.Add(SaveEvery).Before(next) {
		next = m.lastSave.Add(SaveEvery)
	}
	return max(0, next.Sub(now))
}

// shutdown stops the prober and saves the state.
func (m *Monitor) shutdown() {
	m.mu.Lock()
	m.closed = true
	m.invalidateEvidence()
	m.mu.Unlock()
	m.stopSession()
	m.save(m.d.Now())
}

// warnOnce logs msg with err once per distinct error under key; an empty msg
// clears key, so the next failure is logged again.
func (m *Monitor) warnOnce(key, msg string, err error) {
	if msg == "" {
		delete(m.warned, key)
		return
	}
	text := err.Error()
	if m.warned[key] == text {
		return
	}
	m.warned[key] = text
	slog.Warn(msg, "error", err)
}
