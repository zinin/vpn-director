package wizard

import (
	"sync"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

type Step string

const (
	StepNone         Step = ""
	StepSelectServer Step = "select_server"
	StepExclusions   Step = "exclusions"
	StepExcludeIPs   Step = "exclude_ips"
	StepClients      Step = "clients"
	StepClientIP     Step = "client_ip"
	StepClientRoute  Step = "client_route"
	StepConfirm      Step = "confirm"
)

type ClientRoute struct {
	IP    string
	Route string // "xray", "ovpnc1", ..., "wgc5"
}

type State struct {
	mu          sync.RWMutex
	ChatID      int64
	Step        Step
	ServerIndex int
	Picked      *vpnconfig.ActiveServer // the server step 1 picked, as the list read then
	Exclusions  map[string]bool
	ExcludeIPs  []string
	Clients     []ClientRoute
	PendingIP   string
}

// Thread-safe setters
func (s *State) SetStep(step Step) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Step = step
}

func (s *State) SetServerIndex(idx int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ServerIndex = idx
}

// PickServer records the server step 1 picked and where the list had it. Steps
// 2 to 4 take minutes, and meanwhile a refresh - the subscription watch, another
// importer - can move it or drop it: its index then names a server the user
// never chose.
func (s *State) PickServer(idx int, srv vpnconfig.Server) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ServerIndex = idx
	s.Picked = vpnconfig.NewActiveServer(srv)
}

// PickedIndex is where servers has the server step 1 picked, by subscription,
// name, address and port, or -1 when servers no longer has it. A state with no
// pick recorded has only its index to go by.
func (s *State) PickedIndex(servers []vpnconfig.Server) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	inRange := s.ServerIndex >= 0 && s.ServerIndex < len(servers)
	if s.Picked == nil {
		if inRange {
			return s.ServerIndex
		}
		return -1
	}
	if inRange && s.picks(servers[s.ServerIndex]) {
		return s.ServerIndex
	}
	for i, srv := range servers {
		if s.picks(srv) {
			return i
		}
	}
	return -1
}

// PickedName is the name of the server step 1 picked, empty with no pick.
func (s *State) PickedName() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.Picked == nil {
		return ""
	}
	return s.Picked.Name
}

func (s *State) picks(srv vpnconfig.Server) bool {
	return srv.Subscription == s.Picked.Subscription && srv.Name == s.Picked.Name &&
		srv.Address == s.Picked.Address && srv.Port == s.Picked.Port
}

func (s *State) SetExclusion(key string, value bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Exclusions[key] = value
}

func (s *State) ToggleExclusion(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Exclusions[key] = !s.Exclusions[key]
}

func (s *State) SetPendingIP(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.PendingIP = ip
}

func (s *State) AddClient(client ClientRoute) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Clients = append(s.Clients, client)
}

func (s *State) RemoveLastClient() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.Clients) > 0 {
		s.Clients = s.Clients[:len(s.Clients)-1]
	}
}

func (s *State) AddExcludeIP(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ExcludeIPs = append(s.ExcludeIPs, ip)
}

func (s *State) RemoveExcludeIP(index int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if index >= 0 && index < len(s.ExcludeIPs) {
		s.ExcludeIPs = append(s.ExcludeIPs[:index], s.ExcludeIPs[index+1:]...)
	}
}

func (s *State) SetExcludeIPs(ips []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ExcludeIPs = ips
}

func (s *State) GetExcludeIPs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cp := make([]string, len(s.ExcludeIPs))
	copy(cp, s.ExcludeIPs)
	return cp
}

// Thread-safe getters (use RLock for better concurrency)
func (s *State) GetStep() Step {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Step
}

func (s *State) GetServerIndex() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ServerIndex
}

func (s *State) GetPendingIP() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.PendingIP
}

func (s *State) GetExclusions() map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cp := make(map[string]bool)
	for k, v := range s.Exclusions {
		cp[k] = v
	}
	return cp
}

func (s *State) GetClients() []ClientRoute {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cp := make([]ClientRoute, len(s.Clients))
	for i, c := range s.Clients {
		cp[i] = c
	}
	return cp
}

type Manager struct {
	mu     sync.RWMutex
	states map[int64]*State
}

func NewManager() *Manager {
	return &Manager{
		states: make(map[int64]*State),
	}
}

func (m *Manager) Get(chatID int64) *State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.states[chatID]
}

func (m *Manager) Start(chatID int64) *State {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := &State{
		ChatID:     chatID,
		Step:       StepSelectServer,
		Exclusions: make(map[string]bool),
		ExcludeIPs: []string{},
		Clients:    []ClientRoute{},
	}
	m.states[chatID] = state
	return state
}

func (m *Manager) Clear(chatID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.states, chatID)
}
