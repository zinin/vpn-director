package monitor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

func twoEndpoints() []Endpoint {
	return []Endpoint{
		{Key: "k0", Outbound: json.RawMessage(`{"protocol":"trojan","settings":{"servers":[{"address":"192.0.2.1","port":443,"password":"p"}]}}`)},
		{Key: "k1", Outbound: json.RawMessage(`{"protocol":"vless","settings":{"vnext":[{"address":"192.0.2.2","port":443,"users":[{"id":"u","encryption":"none"}]}]},"tag":"proxy-out"}`)},
	}
}

// The golden file is the exact config the prober runs. Regenerate with:
//
//	UPDATE_GOLDEN=1 go test ./internal/monitor -run TestProbeConfig_Golden -count=1
func TestProbeConfig_Golden(t *testing.T) {
	got, err := probeConfig(twoEndpoints(), []account{{User: "e0", Pass: "p0"}, {User: "e1", Pass: "p1"}}, 20000)
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "probe_config.golden.json")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll("testdata", 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, got, 0644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden: %v (regenerate with UPDATE_GOLDEN=1)", err)
	}
	if string(want) != string(got) {
		t.Errorf("config differs from %s:\n%s", golden, got)
	}
}

// Nothing leaves the router directly: traffic no rule names goes to the
// blackhole, Xray's first outbound, and each account reaches its own
// outbound alone.
func TestProbeConfig_EveryAccountReachesItsOwnOutboundAndNothingElse(t *testing.T) {
	raw, err := probeConfig(twoEndpoints(), []account{{User: "e0", Pass: "p0"}, {User: "e1", Pass: "p1"}}, 20000)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Inbounds []struct {
			Listen   string `json:"listen"`
			Port     int    `json:"port"`
			Settings struct {
				Auth string `json:"auth"`
			} `json:"settings"`
		} `json:"inbounds"`
		Outbounds []struct {
			Tag      string `json:"tag"`
			Protocol string `json:"protocol"`
		} `json:"outbounds"`
		Routing struct {
			Rules []struct {
				User        []string `json:"user"`
				OutboundTag string   `json:"outboundTag"`
			} `json:"rules"`
		} `json:"routing"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Outbounds) != 3 || cfg.Outbounds[0].Protocol != "blackhole" || cfg.Outbounds[1].Tag != "m0" || cfg.Outbounds[2].Tag != "m1" {
		t.Fatalf("outbounds %+v", cfg.Outbounds)
	}
	if len(cfg.Inbounds) != 1 || cfg.Inbounds[0].Listen != "127.0.0.1" || cfg.Inbounds[0].Port != 20000 || cfg.Inbounds[0].Settings.Auth != "password" {
		t.Fatalf("inbounds %+v", cfg.Inbounds)
	}
	if len(cfg.Routing.Rules) != 2 || cfg.Routing.Rules[1].User[0] != "e1" || cfg.Routing.Rules[1].OutboundTag != "m1" {
		t.Fatalf("rules %+v", cfg.Routing.Rules)
	}
}

func TestNewAccounts_OnePerEndpointWithItsOwnPassword(t *testing.T) {
	a, err := newAccounts(3)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 3 || a[0].User != "e0" || a[2].User != "e2" || a[0].Pass == a[1].Pass || len(a[0].Pass) != 32 {
		t.Fatalf("accounts %+v", a)
	}
}

// Retagging must preserve the numeric literals the shared validator inspected.
func TestProbeConfig_PreservesValidatedOutboundNumericLiterals(t *testing.T) {
	for _, tc := range []struct {
		name, literal string
		refused       bool
	}{
		{name: "decimal packet-up limit", literal: "8192.0"},
		{name: "integer above 2^53", literal: "9007199254740993"},
		{name: "small integer packet-up limit", literal: "8192", refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := vpnconfig.Server{Name: "Numbers", Address: "numbers.example", Port: 443,
				IPs:      []string{"192.0.2.1"},
				Outbound: json.RawMessage(fmt.Sprintf(`{"protocol":"vless","settings":{"vnext":[{"address":"numbers.example","port":443,"users":[{"id":"u","encryption":"none"}]}]},"streamSettings":{"network":"xhttp","security":"none","xhttpSettings":{"path":"/x","mode":"packet-up","extra":{"scMaxEachPostBytes":%s,"copies":{"object":{"value":%s},"array":[%s,{"value":%s}]}}}},"tag":"stored"}`, tc.literal, tc.literal, tc.literal, tc.literal))}
			eps, refused := Build([]vpnconfig.Subscription{{ID: "0a1b2c3d", Name: "Synthetic", Servers: []vpnconfig.Server{s}}}, nil,
				func(s vpnconfig.Server) (json.RawMessage, error) { return service.OutboundJSON(s, "validated") })
			if tc.refused {
				if len(eps) != 0 || len(refused) != 1 {
					t.Fatalf("endpoints %d, refusals %d; want 0, 1", len(eps), len(refused))
				}
				for _, reason := range refused {
					if !strings.Contains(reason, "scMaxEachPostBytes") {
						t.Fatalf("refusal %q, want the small packet-up refusal", reason)
					}
				}
				return
			}
			if len(eps) != 1 || len(refused) != 0 {
				t.Fatalf("endpoints %d, refusals %d; want 1, 0", len(eps), len(refused))
			}
			want, err := vpnconfig.DecodeOutbound(eps[0].Outbound)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := probeConfig(eps, []account{{User: "e0", Pass: "p0"}}, 20000)
			if err != nil {
				t.Fatal(err)
			}
			var cfg struct {
				Outbounds []json.RawMessage `json:"outbounds"`
			}
			if err := json.Unmarshal(raw, &cfg); err != nil {
				t.Fatal(err)
			}
			if len(cfg.Outbounds) != 2 {
				t.Fatalf("outbounds %d, want blackhole and the validated endpoint", len(cfg.Outbounds))
			}
			got, err := vpnconfig.DecodeOutbound(cfg.Outbounds[1])
			if err != nil {
				t.Fatal(err)
			}
			stream := got["streamSettings"].(map[string]interface{})
			extra := stream["xhttpSettings"].(map[string]interface{})["extra"].(map[string]interface{})
			copies := extra["copies"].(map[string]interface{})
			array := copies["array"].([]interface{})
			for _, value := range []interface{}{
				extra["scMaxEachPostBytes"], copies["object"].(map[string]interface{})["value"],
				array[0], array[1].(map[string]interface{})["value"],
			} {
				if number, ok := value.(json.Number); !ok || number.String() != tc.literal {
					t.Errorf("numeric literal %v, want exactly %s", value, tc.literal)
				}
			}
			if got["tag"] != "m0" || want["tag"] != "validated" {
				t.Errorf("tags %v, %v; want m0, validated", got["tag"], want["tag"])
			}
			delete(got, "tag")
			delete(want, "tag")
			if !reflect.DeepEqual(got, want) {
				t.Error("config outbound differs from its validated input beyond its tag")
			}
		})
	}
}
