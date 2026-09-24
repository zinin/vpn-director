package wizard

import (
	"sync"
	"testing"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

func TestManager_StartAndGet(t *testing.T) {
	m := NewManager()
	chatID := int64(123)

	state := m.Start(chatID)
	if state == nil {
		t.Fatal("expected non-nil state")
	}
	if state.GetStep() != StepSelectServer {
		t.Errorf("expected step %s, got %s", StepSelectServer, state.GetStep())
	}

	// Get same state
	got := m.Get(chatID)
	if got != state {
		t.Error("expected same state instance")
	}
}

func TestManager_GetNonExistent(t *testing.T) {
	m := NewManager()
	if m.Get(999) != nil {
		t.Error("expected nil for non-existent chat")
	}
}

func TestManager_Clear(t *testing.T) {
	m := NewManager()
	chatID := int64(123)

	m.Start(chatID)
	m.Clear(chatID)

	if m.Get(chatID) != nil {
		t.Error("expected nil after clear")
	}
}

func TestState_Step(t *testing.T) {
	s := &State{Exclusions: make(map[string]bool)}
	s.SetStep(StepExclusions)
	if s.GetStep() != StepExclusions {
		t.Errorf("expected %s, got %s", StepExclusions, s.GetStep())
	}
}

func TestState_ServerIndex(t *testing.T) {
	s := &State{Exclusions: make(map[string]bool)}
	s.SetServerIndex(5)
	if s.GetServerIndex() != 5 {
		t.Errorf("expected 5, got %d", s.GetServerIndex())
	}
}

func TestState_PendingIP(t *testing.T) {
	s := &State{Exclusions: make(map[string]bool)}
	s.SetPendingIP("192.168.1.100")
	if s.GetPendingIP() != "192.168.1.100" {
		t.Errorf("expected 192.168.1.100, got %s", s.GetPendingIP())
	}
}

func TestState_Exclusions(t *testing.T) {
	s := &State{Exclusions: make(map[string]bool)}
	s.SetExclusion("ru", true)
	s.SetExclusion("ua", true)
	s.SetExclusion("ru", false)

	excl := s.GetExclusions()
	if excl["ru"] != false {
		t.Error("ru should be false")
	}
	if excl["ua"] != true {
		t.Error("ua should be true")
	}
}

func TestState_ToggleExclusion(t *testing.T) {
	s := &State{Exclusions: make(map[string]bool)}

	// Initially false/missing
	s.ToggleExclusion("ru")
	if !s.GetExclusions()["ru"] {
		t.Error("ru should be true after toggle")
	}

	// Toggle back to false
	s.ToggleExclusion("ru")
	if s.GetExclusions()["ru"] {
		t.Error("ru should be false after second toggle")
	}
}

func TestState_Clients(t *testing.T) {
	s := &State{Exclusions: make(map[string]bool)}
	s.AddClient(ClientRoute{IP: "192.168.1.1", Route: "xray"})
	s.AddClient(ClientRoute{IP: "192.168.1.2", Route: "ovpnc1"})

	clients := s.GetClients()
	if len(clients) != 2 {
		t.Errorf("expected 2 clients, got %d", len(clients))
	}

	if clients[0].IP != "192.168.1.1" || clients[0].Route != "xray" {
		t.Error("first client mismatch")
	}
	if clients[1].IP != "192.168.1.2" || clients[1].Route != "ovpnc1" {
		t.Error("second client mismatch")
	}

	s.RemoveLastClient()
	if len(s.GetClients()) != 1 {
		t.Error("expected 1 client after remove")
	}

	// Remove last remaining
	s.RemoveLastClient()
	if len(s.GetClients()) != 0 {
		t.Error("expected 0 clients after remove")
	}

	// Remove from empty - should not panic
	s.RemoveLastClient()
	if len(s.GetClients()) != 0 {
		t.Error("expected 0 clients")
	}
}

func TestState_GetExclusions_ReturnsCopy(t *testing.T) {
	s := &State{Exclusions: make(map[string]bool)}
	s.SetExclusion("ru", true)

	// Modify returned copy
	excl := s.GetExclusions()
	excl["ru"] = false
	excl["ua"] = true

	// Original should be unchanged
	original := s.GetExclusions()
	if !original["ru"] {
		t.Error("original ru should still be true")
	}
	if original["ua"] {
		t.Error("original should not have ua")
	}
}

func TestState_GetClients_ReturnsCopy(t *testing.T) {
	s := &State{Exclusions: make(map[string]bool)}
	s.AddClient(ClientRoute{IP: "192.168.1.1", Route: "xray"})

	// Modify returned copy
	clients := s.GetClients()
	clients[0].IP = "modified"

	// Original should be unchanged
	original := s.GetClients()
	if original[0].IP != "192.168.1.1" {
		t.Error("original client IP should be unchanged")
	}
}

func TestState_ConcurrentAccess(t *testing.T) {
	s := &State{Exclusions: make(map[string]bool)}
	var wg sync.WaitGroup

	// Concurrent writes
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s.SetExclusion("key", i%2 == 0)
			s.SetServerIndex(i)
			s.SetStep(StepExclusions)
			s.SetPendingIP("192.168.1.1")
			s.AddClient(ClientRoute{IP: "1.1.1.1", Route: "xray"})
			s.ToggleExclusion("toggle")
		}(i)
	}

	// Concurrent reads
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.GetExclusions()
			_ = s.GetServerIndex()
			_ = s.GetClients()
			_ = s.GetStep()
			_ = s.GetPendingIP()
		}()
	}

	wg.Wait()
	// No race detector errors = pass
}

func TestManager_ConcurrentSessions(t *testing.T) {
	m := NewManager()
	var wg sync.WaitGroup

	for i := int64(0); i < 50; i++ {
		wg.Add(1)
		go func(chatID int64) {
			defer wg.Done()
			state := m.Start(chatID)
			state.SetServerIndex(int(chatID))
			state.SetStep(StepExclusions)

			got := m.Get(chatID)
			if got == nil {
				t.Errorf("chatID %d: expected state, got nil", chatID)
				return
			}
			if got.GetServerIndex() != int(chatID) {
				t.Errorf("chatID %d: wrong server index", chatID)
			}

			m.Clear(chatID)
		}(i)
	}

	wg.Wait()
}

func TestManager_StartOverwritesPrevious(t *testing.T) {
	m := NewManager()
	chatID := int64(123)

	// First session
	state1 := m.Start(chatID)
	state1.SetServerIndex(5)

	// Start new session - should overwrite
	state2 := m.Start(chatID)
	if state2.GetServerIndex() != 0 {
		t.Error("new session should have default server index 0")
	}

	// Old state should not be the same object
	if state1 == state2 {
		t.Error("new Start should create new state object")
	}
}

func TestState_PickedIndexTellsSubscriptionsApart(t *testing.T) {
	servers := []vpnconfig.Server{
		{Subscription: "0a1b2c3d", Name: "Germany-1", Address: "de.example.com", Port: 443},
		{Subscription: "1b2c3d4e", Name: "Germany-1", Address: "de.example.com", Port: 443},
	}
	s := NewManager().Start(123)
	s.PickServer(0, servers[1]) // a stale hint: the pick is Beta's

	if got := s.PickedIndex(servers); got != 1 {
		t.Fatalf("PickedIndex %d, want 1", got)
	}
}
