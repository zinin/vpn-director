package subscription

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fixtureDir holds the cases lib/subscription.sh answers to as well
// (router/test/unit/subscription.bats): one decoder reading an entry
// differently from the other fails one of the two suites.
const fixtureDir = "../../../testdata/subscription"

// fixtureResult is what a .want.json holds: the error, or the servers and
// the skips by name and reason. A skip's detail is prose and is not compared.
func fixtureResult(t *testing.T, body string) interface{} {
	t.Helper()
	r, err := Decode(body)
	var v interface{}
	if err != nil {
		v = map[string]interface{}{"error": err.Error()}
	} else {
		servers := []interface{}{}
		for _, s := range r.Servers {
			servers = append(servers, map[string]interface{}{
				"name": s.Name, "address": s.Address, "port": s.Port, "outbound": s.Outbound,
			})
		}
		skipped := []interface{}{}
		for _, s := range r.Skipped {
			skipped = append(skipped, map[string]interface{}{"name": s.Name, "reason": s.Reason})
		}
		v = map[string]interface{}{"total": r.Total, "servers": servers, "skipped": skipped}
	}
	// Through JSON and back, so numbers and nesting compare the way the
	// .want.json reads.
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDecode_Fixtures(t *testing.T) {
	inputs, err := filepath.Glob(filepath.Join(fixtureDir, "*.in"))
	if err != nil || len(inputs) == 0 {
		t.Fatalf("no fixtures in %s: %v", fixtureDir, err)
	}
	for _, in := range inputs {
		name := strings.TrimSuffix(filepath.Base(in), ".in")
		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile(in)
			if err != nil {
				t.Fatal(err)
			}
			wantRaw, err := os.ReadFile(strings.TrimSuffix(in, ".in") + ".want.json")
			if err != nil {
				t.Fatal(err)
			}
			var want interface{}
			if err := json.Unmarshal(wantRaw, &want); err != nil {
				t.Fatalf("%s.want.json: %v", name, err)
			}
			got := fixtureResult(t, string(body))
			if !reflect.DeepEqual(got, want) {
				gotJSON, _ := json.MarshalIndent(got, "", "  ")
				t.Errorf("decoded differently from %s.want.json; got:\n%s", name, gotJSON)
			}
		})
	}
}
