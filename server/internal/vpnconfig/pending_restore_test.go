package vpnconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const pendingRestoreConfigJSON = `{
	"paused_clients": ["192.168.1.9"],
	"tunnel_director": {"tunnels": {
		"ovpnc2": {"clients": ["192.168.1.3", "192.168.1.4"], "exclude": ["zz"]},
		"wgc1": {"clients": ["192.168.1.20"], "exclude": []}
	}},
	"xray": {
		"clients": ["192.168.1.9", "192.168.1.10", "192.168.1.8", "192.168.1.3"],
		"active_server": {"name": "Oslo", "address": "example.com", "port": 443, "subscription": "alpha", "seq": 7},
		"pending_restore": {
			"snapshot": {"tunnel": "ovpnc2", "clients": ["192.168.1.8", "192.168.1.3"], "added": ["192.168.1.8"], "committed": true},
			"restored": ["192.168.1.8", "192.168.1.3"],
			"active": {"name": "Oslo", "address": "example.com", "port": 443, "subscription": "alpha", "seq": 7}
		}
	}
}`

type pendingRestoreRecord struct {
	Snapshot *XrayFailover
	Restored []string
	Active   *ActiveServer
}

func pendingRestoreJSON(t *testing.T, cfg *VPNDirectorConfig) (json.RawMessage, *pendingRestoreRecord) {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Xray struct {
			PendingRestore json.RawMessage `json:"pending_restore"`
		} `json:"xray"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Xray.PendingRestore) == 0 || string(doc.Xray.PendingRestore) == "null" {
		return doc.Xray.PendingRestore, nil
	}
	var record pendingRestoreRecord
	if err := json.Unmarshal(doc.Xray.PendingRestore, &record); err != nil {
		t.Fatal(err)
	}
	return doc.Xray.PendingRestore, &record
}

func TestPendingRestore_RoundTripAndDetach(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vpn-director.json")
	if err := os.WriteFile(path, []byte(pendingRestoreConfigJSON), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadVPNDirectorConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveVPNDirectorConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadVPNDirectorConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, got := pendingRestoreJSON(t, cfg)
	want := &pendingRestoreRecord{
		Snapshot: &XrayFailover{Tunnel: "ovpnc2", Clients: []string{"192.168.1.8", "192.168.1.3"}, Added: []string{"192.168.1.8"}, Committed: true},
		Restored: []string{"192.168.1.8", "192.168.1.3"},
		Active:   &ActiveServer{Name: "Oslo", Address: "example.com", Port: 443, Subscription: "alpha", Seq: 7},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pending restore after file round-trip = %+v, want %+v; JSON %s", got, want, raw)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 3 {
		t.Fatalf("pending restore fields %v, want only snapshot, restored and active", fields)
	}
	for key := range fields {
		switch strings.ToLower(key) {
		case "snapshot", "restored", "active":
		default:
			t.Fatalf("unexpected pending restore metadata field %q", key)
		}
	}

	// pending_restore exists after failover was removed; detaching cannot depend on failover.
	DetachFailoverClient(cfg, "192.168.1.8/32")
	_, got = pendingRestoreJSON(t, cfg)
	want.Snapshot.Clients = []string{"192.168.1.3"}
	want.Snapshot.Added = []string{}
	want.Restored = []string{"192.168.1.3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("detached pending restore = %+v, want %+v", got, want)
	}
	if !reflect.DeepEqual(cfg.Xray.Clients, []string{"192.168.1.9", "192.168.1.10", "192.168.1.8", "192.168.1.3"}) ||
		!reflect.DeepEqual(cfg.TunnelDirector.Tunnels, map[string]TunnelConfig{
			"ovpnc2": {Clients: []string{"192.168.1.3", "192.168.1.4"}, Exclude: []string{"zz"}},
			"wgc1":   {Clients: []string{"192.168.1.20"}, Exclude: []string{}},
		}) || !reflect.DeepEqual(cfg.PausedClients, []string{"192.168.1.9"}) {
		t.Fatalf("DetachFailoverClient changed routing assignments: %+v", cfg)
	}
	if err := SaveVPNDirectorConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadVPNDirectorConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	_, got = pendingRestoreJSON(t, cfg)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("detached intent after file round-trip = %+v, want %+v", got, want)
	}
}

func TestPendingRestore_DetachFiltersBothRecords(t *testing.T) {
	var cfg VPNDirectorConfig
	if err := json.Unmarshal([]byte(pendingRestoreConfigJSON), &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Xray.Failover = &XrayFailover{Tunnel: "ovpnc2", Clients: []string{"192.168.1.8", "192.168.1.3"}, Added: []string{"192.168.1.8"}, Committed: true}
	DetachFailoverClient(&cfg, "192.168.1.8/32")
	_, pending := pendingRestoreJSON(t, &cfg)
	wantSnapshot := &XrayFailover{Tunnel: "ovpnc2", Clients: []string{"192.168.1.3"}, Added: []string{}, Committed: true}
	if !reflect.DeepEqual(cfg.Xray.Failover, wantSnapshot) {
		t.Fatalf("failover = %+v, want %+v", cfg.Xray.Failover, wantSnapshot)
	}
	if pending == nil || !reflect.DeepEqual(pending.Snapshot, wantSnapshot) || !reflect.DeepEqual(pending.Restored, []string{"192.168.1.3"}) {
		t.Fatalf("pending restore = %+v; both records must lose the detached address", pending)
	}
}

func TestPendingRestore_AddedKindsSurviveRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name  string
		added string
		want  []string
	}{
		{"legacy snapshot", "null", nil},
		{"only preexisting tunnel clients", "[]", []string{}},
		{"appended client", `["192.168.1.8"]`, []string{"192.168.1.8"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := strings.Replace(pendingRestoreConfigJSON, `"added": ["192.168.1.8"]`, `"added": `+tc.added, 1)
			var cfg VPNDirectorConfig
			if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
				t.Fatal(err)
			}
			DetachFailoverClient(&cfg, "192.168.1.99")
			_, got := pendingRestoreJSON(t, &cfg)
			if got == nil || got.Snapshot == nil || !reflect.DeepEqual(got.Snapshot.Added, tc.want) {
				t.Fatalf("pending restore = %+v; Added must remain %#v across parsing, detach and save", got, tc.want)
			}
		})
	}
}

func TestPendingRestore_ArmsWithoutSubscriptionsOrClients(t *testing.T) {
	var cfg VPNDirectorConfig
	if err := json.Unmarshal([]byte(pendingRestoreConfigJSON), &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Xray.Clients = nil
	if !Armed(&cfg, 0) {
		t.Fatal("a persistent restore intent must finish even without subscriptions or Xray clients")
	}
	if cfg.Xray.Failover != nil {
		t.Fatal("fixture must be armed by pending restore, not by a failover")
	}
	if Armed(&VPNDirectorConfig{}, 0) {
		t.Fatal("no subscription, clients, failover or pending restore must remain unarmed")
	}
	raw, got := pendingRestoreJSON(t, &VPNDirectorConfig{})
	if len(raw) != 0 || got != nil {
		t.Fatalf("empty config encoded pending_restore %s; nil intent must be omitted", raw)
	}
}
