package netpath

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadiness_UsesOnlyProvidedPaths(t *testing.T) {
	dir := t.TempDir()
	r := Readiness{
		TablesPath:   filepath.Join(dir, "tables"),
		FailoverPath: filepath.Join(dir, "failover"),
		TPROXYPath:   filepath.Join(dir, "tproxy"),
		StoppedPath:  filepath.Join(dir, "stopped"),
	}
	if r.FallbackReady("ovpnc2") || r.TPROXYReady() || r.Stopped() {
		t.Fatal("missing provided files must not be ready or stopped")
	}
	for path, body := range map[string]string{
		r.TablesPath:   "0 ovpnc2\n",
		r.FailoverPath: "ovpnc2\n",
		r.TPROXYPath:   "synthetic-ready\n",
		r.StoppedPath:  "",
	} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if !r.FallbackReady("ovpnc2") || !r.TPROXYReady() || !r.Stopped() {
		t.Fatal("synthetic provided markers must be used")
	}

	other := Readiness{
		TablesPath:   filepath.Join(dir, "other-tables"),
		FailoverPath: filepath.Join(dir, "other-failover"),
		TPROXYPath:   filepath.Join(dir, "other-tproxy"),
		StoppedPath:  filepath.Join(dir, "other-stopped"),
	}
	if other.FallbackReady("ovpnc2") || other.TPROXYReady() || other.Stopped() {
		t.Fatal("another Readiness must not reuse the first one's markers")
	}
	for _, path := range []string{r.FailoverPath, r.TPROXYPath, r.StoppedPath} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	if r.FallbackReady("ovpnc2") || r.TPROXYReady() || r.Stopped() {
		t.Fatal("removed provided markers must not stay ready or stopped")
	}
}

func TestReadiness_FallbackNeedsMatchingTunnelAndMarker(t *testing.T) {
	for _, tc := range []struct {
		name   string
		id     string
		tables *string
		marker *string
		want   bool
	}{
		{"both absent", "ovpnc2", nil, nil, false},
		{"tables alone", "ovpnc2", text("0 ovpnc2\n"), nil, false},
		{"marker alone", "ovpnc2", nil, text("ovpnc2\n"), false},
		{"unlisted tunnel", "ovpnc2", text("0 wgc1\n"), text("ovpnc2\n"), false},
		{"different marker", "ovpnc2", text("0 ovpnc2\n"), text("wgc1\n"), false},
		{"empty marker", "ovpnc2", text("0 ovpnc2\n"), text(" \n"), false},
		{"empty id", "", text("0 ovpnc2\n"), text("\n"), false},
		{"matching marker", "ovpnc2", text("0 ovpnc2\n"), text("ovpnc2\n"), true},
		{"trimmed marker", "ovpnc2", text("0 ovpnc2\n"), text(" \tovpnc2\n"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			r := Readiness{TablesPath: filepath.Join(dir, "tables"), FailoverPath: filepath.Join(dir, "failover")}
			for path, body := range map[string]*string{r.TablesPath: tc.tables, r.FailoverPath: tc.marker} {
				if body != nil {
					if err := os.WriteFile(path, []byte(*body), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if got := r.FallbackReady(tc.id); got != tc.want {
				t.Fatalf("ready=%t, want %t", got, tc.want)
			}
		})
	}
}

func TestReadiness_TPROXYNeedsNonEmptyMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tproxy")
	r := Readiness{TPROXYPath: path}
	if r.TPROXYReady() {
		t.Fatal("missing marker must not be ready")
	}
	for _, body := range []string{"", " \t\n", "synthetic-ready\n"} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		want := body == "synthetic-ready\n"
		if got := r.TPROXYReady(); got != want {
			t.Fatalf("marker %q: ready=%t, want %t", body, got, want)
		}
	}
}

func TestReadiness_EmptyPathsAreNotReady(t *testing.T) {
	r := Readiness{}
	if r.FallbackReady("ovpnc2") || r.TPROXYReady() || r.Stopped() {
		t.Fatal("empty paths must not select router defaults")
	}
}

func text(s string) *string { return &s }
