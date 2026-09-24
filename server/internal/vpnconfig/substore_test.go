package vpnconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func writeSubFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// testdata/substore is read by the bats suite too (router/test/unit/substore.bats):
// both sides list the same subscriptions in the same order.
func TestLoadSubscriptions_ReadsTheSharedFixtures(t *testing.T) {
	subs, err := LoadSubscriptions(filepath.Join("..", "..", "..", "testdata", "substore"))
	if err != nil {
		t.Fatal(err)
	}
	var ids, names []string
	for _, s := range subs {
		ids = append(ids, s.ID)
		names = append(names, s.Name)
	}
	// Ordered by added, then by id: Beta came first, Alpha and Gamma at one
	// moment, and 0a1b2c3d sorts before 2c3d4e5f.
	if want := []string{"1b2c3d4e", "0a1b2c3d", "2c3d4e5f"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids %v, want %v", ids, want)
	}
	if want := []string{"Beta", "Alpha", "Gamma"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("names %v, want %v", names, want)
	}
	if !subs[0].Static() || subs[1].Static() {
		t.Fatalf("static: Beta %v, Alpha %v", subs[0].Static(), subs[1].Static())
	}
	if subs[1].Host() != "sub.example.com" || subs[1].Error != "download failed: HTTP 403" {
		t.Fatalf("Alpha: host %q, error %q", subs[1].Host(), subs[1].Error)
	}
	for _, sub := range subs {
		for _, s := range sub.Servers {
			if s.Subscription != sub.ID {
				t.Fatalf("server %q of %s carries subscription %q", s.Name, sub.ID, s.Subscription)
			}
		}
	}
	want := []string{"192.0.2.10", "192.0.2.11", "198.51.100.20", "203.0.113.30"}
	if got := SubscriptionIPs(subs); !reflect.DeepEqual(got, want) {
		t.Fatalf("ips %v, want %v", got, want)
	}
}

func TestLoadSubscriptions_AMissingDirectoryHoldsNone(t *testing.T) {
	subs, err := LoadSubscriptions(filepath.Join(t.TempDir(), "subscriptions"))
	if err != nil || subs != nil {
		t.Fatalf("got %v, %v; want none and no error", subs, err)
	}
}

// Only <8 hex digits>.json is a subscription. The temp file of an atomic
// write, a hand-made backup, a directory and a file whose id is not its name
// are not, and none of them may fail the readers of the good one.
func TestLoadSubscriptions_SkipsWhatIsNoSubscription(t *testing.T) {
	dir := t.TempDir()
	good := `{"id":"0a1b2c3d","name":"Alpha","url":"https://sub.example.com/s/t","added":"2026-09-24T18:00:00Z","refreshed":"2026-09-24T18:00:00Z","servers":[]}`
	writeSubFile(t, dir, "0a1b2c3d.json", good)
	writeSubFile(t, dir, ".0a1b2c3d.json.tmp-123", good)
	writeSubFile(t, dir, "0a1b2c3d.json.Ab12Cd", good)
	writeSubFile(t, dir, "0A1B2C3D.json", strings.Replace(good, "0a1b2c3d", "0A1B2C3D", 1))
	writeSubFile(t, dir, "backup.json", good)
	writeSubFile(t, dir, "1b2c3d4e.json", `{"id":"1b2c3d4e",`)
	writeSubFile(t, dir, "2c3d4e5f.json", strings.Replace(good, "0a1b2c3d", "ffffffff", 1))
	if err := os.Mkdir(filepath.Join(dir, "3d4e5f6a.json"), 0700); err != nil {
		t.Fatal(err)
	}

	subs, err := LoadSubscriptions(dir)

	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 1 || subs[0].ID != "0a1b2c3d" {
		t.Fatalf("got %+v, want 0a1b2c3d alone", subs)
	}
}

// captureLog sends slog to a buffer for the rest of the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// A file that cannot be read - a <id>.json that is a symlink to a directory
// reads as EISDIR - is skipped as one that does not parse is: the readers of
// the others do not fail with it.
func TestLoadSubscriptions_AFileThatCannotBeReadIsSkipped(t *testing.T) {
	dir := t.TempDir()
	writeSubFile(t, dir, "0a1b2c3d.json", `{"id":"0a1b2c3d","name":"Alpha","added":"2026-09-24T18:00:00Z","refreshed":"2026-09-24T18:00:00Z","servers":[]}`)
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "1b2c3d4e.json")); err != nil {
		t.Fatal(err)
	}
	logs := captureLog(t)

	subs, err := LoadSubscriptions(dir)

	if err != nil || len(subs) != 1 || subs[0].ID != "0a1b2c3d" {
		t.Fatalf("got %+v, %v; want 0a1b2c3d alone", subs, err)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "file=1b2c3d4e.json") {
		t.Fatalf("no warning names the file: %s", logs)
	}
}

// The watch reads every file each 30 s tick. A file broken by hand is warned
// about once in each state it is found in, not on every read, and again once
// it has changed.
func TestLoadSubscriptions_WarnsOnceForEachStateOfABrokenFile(t *testing.T) {
	dir := t.TempDir()
	writeSubFile(t, dir, "1b2c3d4e.json", `{"id":"1b2c3d4e",`)
	logs := captureLog(t)

	for i := 0; i < 3; i++ {
		if _, err := LoadSubscriptions(dir); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
		t.Fatalf("%d warnings for three reads of one broken file, want 1:\n%s", n, logs)
	}

	writeSubFile(t, dir, "1b2c3d4e.json", `{"id":"ffffffff","name":"Beta"}`)
	for i := 0; i < 2; i++ {
		if _, err := LoadSubscriptions(dir); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 2 {
		t.Fatalf("%d warnings in all after an edit, want 2:\n%s", n, logs)
	}
}

func TestSaveSubscription_WritesAFileOnlyItsOwnerReads(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "subscriptions")
	at := time.Date(2026, 9, 24, 18, 0, 0, 0, time.UTC)
	sub := Subscription{ID: "0a1b2c3d", Name: "Alpha", URL: "https://sub.example.com/s/t", Added: at, Refreshed: at,
		Servers: []Server{{Name: "Oslo", Address: "a.example.com", Port: 443, IPs: []string{"192.0.2.10"}, Subscription: "0a1b2c3d"}}}

	if err := SaveSubscription(dir, sub); err != nil {
		t.Fatal(err)
	}

	for path, want := range map[string]os.FileMode{dir: 0700, filepath.Join(dir, "0a1b2c3d.json"): 0600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("%s: mode %o, want %o", path, got, want)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "0a1b2c3d.json"))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["added"] != "2026-09-24T18:00:00Z" {
		t.Fatalf("added %v, want RFC 3339 UTC seconds", raw["added"])
	}
	// The file names its subscription once; its servers do not repeat it.
	if strings.Contains(string(data), `"subscription"`) {
		t.Fatalf("a server repeats its subscription: %s", data)
	}
	subs, err := LoadSubscriptions(dir)
	if err != nil || len(subs) != 1 || subs[0].Servers[0].Subscription != "0a1b2c3d" || !subs[0].Added.Equal(at) {
		t.Fatalf("read back %+v, %v", subs, err)
	}
}

func TestSaveSubscription_RefusesAnIDTheStoreWouldNotRead(t *testing.T) {
	for _, id := range []string{"", "0A1B2C3D", "0a1b2c3", "0a1b2c3d0", "../0a1b2c"} {
		if err := SaveSubscription(t.TempDir(), Subscription{ID: id}); err == nil {
			t.Fatalf("id %q was saved", id)
		}
	}
}

func TestDeleteSubscriptionFile_AFileAlreadyGoneIsNoError(t *testing.T) {
	dir := t.TempDir()
	writeSubFile(t, dir, "0a1b2c3d.json", "{}")
	for i := 0; i < 2; i++ {
		if err := DeleteSubscriptionFile(dir, "0a1b2c3d"); err != nil {
			t.Fatalf("delete %d: %v", i, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "0a1b2c3d.json")); !os.IsNotExist(err) {
		t.Fatal("the file is still there")
	}
}

func TestRemoveLegacyServers(t *testing.T) {
	dir := t.TempDir()
	writeSubFile(t, dir, "servers.json", "[]")
	for i := 0; i < 2; i++ {
		if err := RemoveLegacyServers(dir); err != nil {
			t.Fatalf("removal %d: %v", i, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "servers.json")); !os.IsNotExist(err) {
		t.Fatal("servers.json is still there")
	}
}

func TestCleanSubscriptionName(t *testing.T) {
	long := strings.Repeat("я", MaxSubscriptionName)
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{"  Alpha  ", "Alpha", true},
		{"Бета VPN", "Бета VPN", true},
		{long, long, true}, // 32 characters, 64 bytes
		{long + "я", "", false},
		{"   ", "", false},
		{"Al\tpha", "", false},
		{"\tAlpha", "", false}, // only spaces are trimmed; a tab is a control character
		{"Al\u0085pha", "", false},
		{"Al\x7fpha", "", false},
		{"\xffAlpha", "", false},
	} {
		got, err := CleanSubscriptionName(tc.in)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("CleanSubscriptionName(%q) = %q, %v; want %q, ok=%v", tc.in, got, err, tc.want, tc.ok)
		}
		if err != nil && !errors.Is(err, ErrSubscriptionName) {
			t.Errorf("CleanSubscriptionName(%q): %v is not ErrSubscriptionName", tc.in, err)
		}
	}
}

// jq's ascii_downcase is the fold the shell applies, and the daemons have to
// agree with it on which names collide (router/test/unit/substore.bats asks
// the same questions).
func TestSubscriptionNameTaken_FoldsASCIIOnly(t *testing.T) {
	subs := []Subscription{{ID: "0a1b2c3d", Name: "Beta"}, {ID: "1b2c3d4e", Name: "Бета"}}
	for _, tc := range []struct {
		name, except string
		taken        bool
	}{
		{"beta", "", true},
		{"BETA", "", true},
		{"beta", "0a1b2c3d", false}, // its own name
		{"бета", "", false},         // Cyrillic is not folded
		{"Бета", "", true},
		{"Gamma", "", false},
	} {
		if got := SubscriptionNameTaken(subs, tc.name, tc.except); got != tc.taken {
			t.Errorf("SubscriptionNameTaken(%q, except %q) = %v, want %v", tc.name, tc.except, got, tc.taken)
		}
	}
}

func TestDefaultSubscriptionName(t *testing.T) {
	host40 := strings.Repeat("a", 36) + ".com"
	for _, tc := range []struct {
		taken []string
		base  string
		want  string
	}{
		{nil, "sub.example.com", "sub.example.com"},
		{[]string{"Sub.Example.com"}, "sub.example.com", "sub.example.com-2"},
		{[]string{"sub.example.com", "sub.example.com-2"}, "sub.example.com", "sub.example.com-3"},
		{nil, host40, host40[:32]},
		{[]string{host40[:32]}, host40, host40[:30] + "-2"},
		{nil, strings.Repeat("a", 31) + " bc", strings.Repeat("a", 31)},
		{nil, "  list\x07.txt ", "list.txt"},
		{nil, "", "subscription"},
		{nil, " \x01 ", "subscription"},
	} {
		var subs []Subscription
		for i, n := range tc.taken {
			subs = append(subs, Subscription{ID: fmt.Sprintf("%08x", i), Name: n})
		}
		if got := DefaultSubscriptionName(subs, tc.base); got != tc.want {
			t.Errorf("DefaultSubscriptionName(%v, %q) = %q, want %q", tc.taken, tc.base, got, tc.want)
		}
	}
}

func TestNewSubscriptionID_IsValidAndFree(t *testing.T) {
	subs := []Subscription{{ID: "0a1b2c3d"}}
	for i := 0; i < 100; i++ {
		id, err := NewSubscriptionID(subs)
		if err != nil {
			t.Fatal(err)
		}
		if !ValidSubscriptionID(id) || id == "0a1b2c3d" {
			t.Fatalf("id %q", id)
		}
	}
}

func TestAllServers_KeepsSubscriptionOrderAndMarksEachServer(t *testing.T) {
	subs := []Subscription{
		{ID: "0a1b2c3d", Servers: []Server{{Name: "A1", IPs: []string{"192.0.2.2"}}, {Name: "A2", IPs: []string{"192.0.2.1", ""}}}},
		{ID: "1b2c3d4e", Servers: []Server{{Name: "B1", IPs: []string{"192.0.2.1"}}}},
	}

	all := AllServers(subs)

	var got []string
	for _, s := range all {
		got = append(got, s.Subscription+"/"+s.Name)
	}
	if want := []string{"0a1b2c3d/A1", "0a1b2c3d/A2", "1b2c3d4e/B1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if ips := SubscriptionIPs(subs); !reflect.DeepEqual(ips, []string{"192.0.2.1", "192.0.2.2"}) {
		t.Fatalf("ips %v", ips)
	}
	if subs[0].Servers[0].Subscription != "" {
		t.Fatal("AllServers changed the subscriptions it read")
	}
	if FindSubscription(subs, "1b2c3d4e") != 1 || FindSubscription(subs, "ffffffff") != -1 {
		t.Fatal("FindSubscription")
	}
}

func TestSubscriptionHost(t *testing.T) {
	for raw, want := range map[string]string{
		"https://sub.example.com/s/token":      "sub.example.com",
		"https://sub.example.com:8443/s/token": "sub.example.com",
		"":                                     "",
	} {
		if got := (Subscription{URL: raw}).Host(); got != want {
			t.Errorf("Host of %q = %q, want %q", raw, got, want)
		}
	}
}
