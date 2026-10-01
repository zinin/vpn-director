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
