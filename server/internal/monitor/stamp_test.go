package monitor

import (
	"os"
	"path/filepath"
	"testing"
)

// A writer replaces a file by a rename; the stamp sees it even at the same
// size and within the same second, as JFFS2 keeps modification times.
func TestStamp_SeesARenameAtTheSameSizeAndTime(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "0a1b2c3d.json")
	cfg := filepath.Join(dir, "vpn-director.json")
	for _, p := range []string{sub, cfg} {
		if err := os.WriteFile(p, []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	before := Stamp(dir, cfg)
	if Stamp(dir, cfg) != before {
		t.Fatal("two stamps of untouched files differ")
	}

	info, _ := os.Stat(sub)
	tmp := sub + ".tmp"
	if err := os.WriteFile(tmp, []byte("[]"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tmp, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, sub); err != nil {
		t.Fatal(err)
	}
	if Stamp(dir, cfg) == before {
		t.Fatal("the stamp missed a replaced file")
	}
}

func TestStamp_LiteralDirectoriesTrackOnlyJSONFiles(t *testing.T) {
	for _, name := range []string{"data[1]", "data*", "data?", "data["} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), name)
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			cfg := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(cfg, []byte("{}"), 0600); err != nil {
				t.Fatal(err)
			}
			before := Stamp(dir, cfg)
			sub := filepath.Join(dir, "0a1b2c3d.json")
			if err := os.WriteFile(sub, []byte("{}"), 0600); err != nil {
				t.Fatal(err)
			}
			added := Stamp(dir, cfg)
			if added == before {
				t.Error("subscription-only add was missed")
			}
			info, err := os.Stat(sub)
			if err != nil {
				t.Fatal(err)
			}
			tmp := sub + ".tmp"
			if err := os.WriteFile(tmp, []byte("[]"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(tmp, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
			if Stamp(dir, cfg) != added {
				t.Error("non-JSON temp invalidated stamp")
			}
			decoy := filepath.Join(filepath.Dir(dir), "dataX")
			if err := os.Mkdir(decoy, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(decoy, "1b2c3d4e.json"), []byte("{}"), 0600); err != nil {
				t.Fatal(err)
			}
			if Stamp(dir, cfg) != added {
				t.Error("stamp enumerated a neighbouring directory")
			}
			if err := os.Rename(tmp, sub); err != nil {
				t.Fatal(err)
			}
			replaced := Stamp(dir, cfg)
			if replaced == added {
				t.Error("same-size/time replacement was missed")
			}
			if err := os.Remove(sub); err != nil {
				t.Fatal(err)
			}
			if Stamp(dir, cfg) == replaced {
				t.Error("subscription-only delete was missed")
			}
		})
	}
}

func TestStamp_AFailedListingIsNotCacheable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "subscriptions")
	missing := Stamp(dir)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	empty := Stamp(dir)
	if empty == missing {
		t.Error("missing and empty directories have the same stamp")
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if Stamp(dir) != "" {
		t.Error("failed listing returned a cacheable stamp")
	}
}
