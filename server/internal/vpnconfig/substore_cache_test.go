package vpnconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func countedSubscriptionCache() (*SubscriptionCache, map[string]int, map[string]int) {
	cache := NewSubscriptionCache()
	reads, parses := map[string]int{}, map[string]int{}
	var current string
	cache.readFile = func(path string) ([]byte, error) {
		current = filepath.Base(path)
		reads[current]++
		return os.ReadFile(path)
	}
	cache.parse = func(data []byte, sub *Subscription) error {
		parses[current]++
		return json.Unmarshal(data, sub)
	}
	return cache, reads, parses
}

func TestSubscriptionCache_ReadsAndParsesOnlyChangedFiles(t *testing.T) {
	dir := t.TempDir()
	at := time.Unix(1720000000, 0).UTC()
	for i, id := range []string{"0a1b2c3d", "1b2c3d4e"} {
		if err := SaveSubscription(dir, Subscription{ID: id, Name: id, Added: at.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	cache, reads, parses := countedSubscriptionCache()
	load := func() []Subscription {
		t.Helper()
		subs, err := cache.Load(dir)
		if err != nil {
			t.Fatal(err)
		}
		return subs
	}
	before := load()
	if got := load(); !reflect.DeepEqual(got, before) {
		t.Fatal("an unchanged load differs")
	}
	config := filepath.Join(t.TempDir(), "vpn-director.json")
	if err := os.WriteFile(config, []byte(`{"xray":{"active_server":{}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	load()
	want := map[string]int{"0a1b2c3d.json": 1, "1b2c3d4e.json": 1}
	if !reflect.DeepEqual(reads, want) || !reflect.DeepEqual(parses, want) {
		t.Fatalf("unchanged/config-only loads: reads=%v parses=%v, want %v", reads, parses, want)
	}

	path := filepath.Join(dir, "1b2c3d4e.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tmp := path + ".tmp"
	data = []byte(strings.Replace(string(data), `"name": "1b2c3d4e"`, `"name": "changed!"`, 1))
	if int64(len(data)) != info.Size() {
		t.Fatal("replacement changed the size")
	}
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tmp, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	after := load()
	load()
	want["1b2c3d4e.json"] = 2
	if after[1].Name != "changed!" || !reflect.DeepEqual(reads, want) || !reflect.DeepEqual(parses, want) {
		t.Fatalf("one replacement: reads=%v parses=%v, want %v; changed=%v", reads, parses, want, after[1].Name == "changed!")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if len(load()) != 1 {
		t.Fatal("a deleted subscription remained cached")
	}
	if err := SaveSubscription(dir, before[1]); err != nil {
		t.Fatal(err)
	}
	load()
	want["1b2c3d4e.json"] = 3
	if !reflect.DeepEqual(reads, want) || !reflect.DeepEqual(parses, want) {
		t.Fatalf("re-added file: reads=%v parses=%v, want %v", reads, parses, want)
	}
}

func TestSubscriptionCache_UsesSharedFilteringOrderingIDsAndWarnings(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{"1b2c3d4e", "0a1b2c3d"} {
		if err := SaveSubscription(dir, Subscription{ID: id, Name: id, Servers: []Server{{Name: "synthetic"}}}); err != nil {
			t.Fatal(err)
		}
	}
	writeSubFile(t, dir, "2c3d4e5f.json", `{"id":"ffffffff"}`)
	writeSubFile(t, dir, "3d4e5f6a.json", `{`)
	writeSubFile(t, dir, "backup.json", `{`)
	writeSubFile(t, dir, ".0a1b2c3d.json.tmp", `{`)
	if err := os.Mkdir(filepath.Join(dir, "4e5f6a7b.json"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "5f6a7b8c.json")); err != nil {
		t.Fatal(err)
	}
	logs := captureLog(t)
	want, err := LoadSubscriptions(dir)
	if err != nil {
		t.Fatal(err)
	}
	cache, reads, parses := countedSubscriptionCache()
	for i := 0; i < 3; i++ {
		got, err := cache.Load(dir)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("cached/shared mismatch or error: %v", err)
		}
	}
	if len(want) != 2 || want[0].ID != "0a1b2c3d" || want[0].Servers[0].Subscription != want[0].ID {
		t.Fatal("shared ordering or server subscription IDs changed")
	}
	wantReads := map[string]int{"0a1b2c3d.json": 1, "1b2c3d4e.json": 1, "2c3d4e5f.json": 1, "3d4e5f6a.json": 1, "5f6a7b8c.json": 1}
	wantParses := map[string]int{"0a1b2c3d.json": 1, "1b2c3d4e.json": 1, "2c3d4e5f.json": 1, "3d4e5f6a.json": 1}
	if !reflect.DeepEqual(reads, wantReads) || !reflect.DeepEqual(parses, wantParses) {
		t.Fatalf("reads=%v parses=%v, want reads=%v parses=%v", reads, parses, wantReads, wantParses)
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 3 {
		t.Fatalf("warnings=%d, want shared one-per-broken-file warnings", n)
	}
	for i := 0; i < 2; i++ {
		if _, err := LoadSubscriptions(dir); err != nil {
			t.Fatal(err)
		}
	}
	if err := SaveSubscription(dir, Subscription{ID: "3d4e5f6a", Name: "repaired"}); err != nil {
		t.Fatal(err)
	}
	got, err := cache.Load(dir)
	if err != nil || len(got) != 3 || reads["3d4e5f6a.json"] != 2 || parses["3d4e5f6a.json"] != 2 {
		t.Fatal("a repaired broken file was not re-parsed")
	}
}

func TestSubscriptionCache_RetriesOneShotReadFailureWithoutFileChanges(t *testing.T) {
	for _, warm := range []bool{false, true} {
		t.Run(map[bool]string{false: "cold", true: "warm"}[warm], func(t *testing.T) {
			dir := t.TempDir()
			stable := Subscription{ID: "0a1b2c3d", Name: "stable"}
			flaky := Subscription{ID: "1b2c3d4e", Name: "recovering"}
			if err := SaveSubscription(dir, stable); err != nil {
				t.Fatal(err)
			}
			cache, reads, parses := countedSubscriptionCache()
			if warm {
				if subs, err := cache.Load(dir); err != nil || len(subs) != 1 {
					t.Fatalf("prime cache: count=%d err=%v", len(subs), err)
				}
			}
			if err := SaveSubscription(dir, flaky); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, flaky.ID+".json")
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			config := filepath.Join(t.TempDir(), "vpn-director.json")
			raw := []byte(`{"data_dir":"data","xray":{}}`)
			if err := os.WriteFile(config, raw, 0600); err != nil {
				t.Fatal(err)
			}
			configBefore, err := os.Stat(config)
			if err != nil {
				t.Fatal(err)
			}
			read := cache.readFile
			fail := true
			cache.readFile = func(name string) ([]byte, error) {
				if name == path && fail {
					fail = false
					reads[filepath.Base(name)]++
					return nil, &os.PathError{Op: "read", Path: name, Err: syscall.EIO}
				}
				return read(name)
			}
			if subs, err := cache.Load(dir); !errors.Is(err, syscall.EIO) || subs != nil {
				t.Fatalf("retryable read published a partial success: count=%d err=%v", len(subs), err)
			}
			for i := 0; i < 2; i++ {
				subs, err := cache.Load(dir)
				if err != nil || len(subs) != 2 || subs[1].ID != flaky.ID {
					t.Fatalf("unchanged file did not recover: count=%d err=%v", len(subs), err)
				}
			}
			after, err := os.Stat(path)
			if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
				t.Fatal("recovery changed subscription inode, size or mtime")
			}
			configAfter, err := os.Stat(config)
			if err != nil || !os.SameFile(configBefore, configAfter) || configBefore.Size() != configAfter.Size() || !configBefore.ModTime().Equal(configAfter.ModTime()) {
				t.Fatal("recovery changed config inode, size or mtime")
			}
			if data, err := os.ReadFile(config); err != nil || !bytes.Equal(data, raw) {
				t.Fatal("recovery changed config content")
			}
			wantReads := map[string]int{"0a1b2c3d.json": 1, "1b2c3d4e.json": 2}
			wantParses := map[string]int{"0a1b2c3d.json": 1, "1b2c3d4e.json": 1}
			if !reflect.DeepEqual(reads, wantReads) || !reflect.DeepEqual(parses, wantParses) {
				t.Fatalf("recovery reads=%v parses=%v, want reads=%v parses=%v", reads, parses, wantReads, wantParses)
			}
		})
	}
}

func TestSubscriptionCache_DirectorySwitchAndMissingDirectory(t *testing.T) {
	cache, reads, parses := countedSubscriptionCache()
	one, two := t.TempDir(), t.TempDir()
	for _, dir := range []string{one, two} {
		if err := SaveSubscription(dir, Subscription{ID: "0a1b2c3d", Name: dir}); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{one, two, one} {
		subs, err := cache.Load(dir)
		if err != nil || len(subs) != 1 || subs[0].Name != dir {
			t.Fatal("a directory switch reused another directory's record")
		}
	}
	if reads["0a1b2c3d.json"] != 3 || parses["0a1b2c3d.json"] != 3 {
		t.Fatalf("switched directories: reads=%v parses=%v", reads, parses)
	}
	if subs, err := cache.Load(filepath.Join(t.TempDir(), "missing")); err != nil || subs != nil {
		t.Fatal("a missing directory must hold no subscriptions")
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Load(file); err == nil {
		t.Fatal("a directory read error was swallowed")
	}
}
