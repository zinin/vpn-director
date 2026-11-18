package updater

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseManifest(t *testing.T) {
	in := "# comment\ncommon   router/opt/vpn-director/vpn-director.sh\n\n  merlin router/jffs/scripts/firewall-start\n"
	got, err := parseManifest(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parseManifest() error = %v", err)
	}
	want := []ManifestEntry{
		{Tag: "common", Path: "router/opt/vpn-director/vpn-director.sh"},
		{Tag: "merlin", Path: "router/jffs/scripts/firewall-start"},
	}
	if len(got) != len(want) {
		t.Fatalf("parseManifest() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseManifest_RejectsMalformedLines(t *testing.T) {
	// The manifest comes from the network and its paths end up in a script
	// that runs as root, so anything but "<tag> router/<clean path>" is refused.
	for _, line := range []string{
		"common",
		"common router/a extra",
		"common ../etc/passwd",
		"common router/opt/../../etc/passwd",
		"common /opt/vpn-director/x.sh",
		"common router/opt/with space.sh",
	} {
		if _, err := parseManifest(strings.NewReader(line + "\n")); err == nil {
			t.Errorf("parseManifest(%q) accepted a malformed line", line)
		}
	}
}

func TestManifestFilesFor(t *testing.T) {
	entries := []ManifestEntry{
		{Tag: "common", Path: "router/a"},
		{Tag: "keenetic", Path: "router/k"},
		{Tag: "merlin", Path: "router/m"},
		{Tag: "common", Path: "router/b"},
	}
	got := manifestFilesFor(entries, "merlin")
	want := []string{"router/a", "router/m", "router/b"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("manifestFilesFor(merlin) = %v, want %v", got, want)
	}
	got = manifestFilesFor(entries, "keenetic")
	want = []string{"router/a", "router/k", "router/b"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("manifestFilesFor(keenetic) = %v, want %v", got, want)
	}
}

func TestIsExecutable(t *testing.T) {
	cases := map[string]bool{
		"router/opt/vpn-director/lib/common.sh":              true,
		"router/opt/etc/init.d/S99vpn-director":              true,
		"router/jffs/scripts/wan-event":                      true,
		"router/opt/vpn-director/vpn-director.json.template": false,
		"router/opt/etc/xray/config.json.template":           false,
		"router/opt/vpn-director/data/servers.json":          false,
		"router/files.manifest":                              false,
	}
	for path, want := range cases {
		if got := isExecutable(path); got != want {
			t.Errorf("isExecutable(%q) = %v, want %v", path, got, want)
		}
	}
}

// TestRepoManifest_EntriesExist pins the manifest to the working tree: a
// renamed or removed file would otherwise break every future update.
func TestRepoManifest_EntriesExist(t *testing.T) {
	f, err := os.Open(filepath.Join("..", "..", "..", manifestPath))
	if err != nil {
		t.Fatalf("open repo manifest: %v", err)
	}
	defer f.Close()
	entries, err := parseManifest(f)
	if err != nil {
		t.Fatalf("parse repo manifest: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("repo manifest is empty")
	}
	for _, e := range entries {
		if _, err := os.Stat(filepath.Join("..", "..", "..", e.Path)); err != nil {
			t.Errorf("manifest entry %q not found in the repo: %v", e.Path, err)
		}
	}
}

// TestRepoManifest_ShipsUpdateCriticalFiles: an update that leaves an old init
// script or the CLI behind is worse than no update.
func TestRepoManifest_ShipsUpdateCriticalFiles(t *testing.T) {
	f, err := os.Open(filepath.Join("..", "..", "..", manifestPath))
	if err != nil {
		t.Fatalf("open repo manifest: %v", err)
	}
	defer f.Close()
	entries, err := parseManifest(f)
	if err != nil {
		t.Fatalf("parse repo manifest: %v", err)
	}
	have := map[string]string{}
	for _, e := range entries {
		have[e.Path] = e.Tag
	}
	for _, want := range []string{
		"router/opt/vpn-director/vpn-director.sh",
		"router/opt/etc/init.d/S99vpn-director",
		"router/opt/etc/init.d/S98telegram-bot",
		"router/opt/etc/init.d/S98vpn-director-webui",
		"router/opt/etc/xray/config.json.template",
	} {
		if have[want] != "common" {
			t.Errorf("manifest must ship %s with tag common, got %q", want, have[want])
		}
	}
	if have["router/opt/vpn-director/hooks/firewall-start"] != "merlin" {
		t.Errorf("hooks/firewall-start must be tagged merlin, got %q", have["router/opt/vpn-director/hooks/firewall-start"])
	}
	// Merlin's /jffs/scripts hooks are shared with the owner and other add-ons:
	// they get one line of ours (vpn-director.sh hooks), and nothing copies a
	// file over them.
	for path := range have {
		if strings.HasPrefix(path, "router/jffs/") {
			t.Errorf("%s would be copied into /jffs over a shared hook", path)
		}
	}
	if have["router/opt/vpn-director/lib/platform/merlin.sh"] != "merlin" {
		t.Errorf("platform/merlin.sh must be tagged merlin, got %q", have["router/opt/vpn-director/lib/platform/merlin.sh"])
	}
}

// TestRepoManifest_ShipsEveryRouterFile pins the tree to the manifest - the
// direction TestRepoManifest_EntriesExist does not cover. The manifest is now
// the only list of shipped files, so a file added under router/ and forgotten
// here ships to nobody, on every platform, and nothing else notices.
func TestRepoManifest_ShipsEveryRouterFile(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	f, err := os.Open(filepath.Join(root, manifestPath))
	if err != nil {
		t.Fatalf("open repo manifest: %v", err)
	}
	defer f.Close()
	entries, err := parseManifest(f)
	if err != nil {
		t.Fatalf("parse repo manifest: %v", err)
	}
	listed := map[string]int{}
	for _, e := range entries {
		listed[e.Path]++
	}

	routerDir := filepath.Join(root, "router")
	testDir := filepath.Join(routerDir, "test")
	walked := 0
	err = filepath.WalkDir(routerDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// The Bats suite is not shipped to routers.
			if path == testDir {
				return fs.SkipDir
			}
			return nil
		}
		rel := filepath.ToSlash(path[len(root)+1:])
		if rel == manifestPath {
			return nil
		}
		walked++
		switch n := listed[rel]; n {
		case 1:
		case 0:
			t.Errorf("%s is not listed in %s, so no router would ever receive it", rel, manifestPath)
		default:
			t.Errorf("%s is listed %d times in %s, want once", rel, n, manifestPath)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk router/: %v", err)
	}
	if walked == 0 {
		t.Fatalf("walked no files under %s", routerDir)
	}
	// Every entry exists (TestRepoManifest_EntriesExist) and every shipped file
	// is listed once, so the two counts can only differ if the manifest lists
	// something this walk skips: a file under router/test/, or the manifest.
	if walked != len(entries) {
		t.Errorf("%d shipped files under router/, %d manifest entries", walked, len(entries))
	}
}

func TestFileEntries(t *testing.T) {
	entries := []ManifestEntry{
		{Tag: "common", Path: "router/opt/vpn-director/lib/common.sh"},
		{Tag: "common", Path: "router/opt/vpn-director/vpn-director.json.template"},
		{Tag: "merlin", Path: "router/jffs/scripts/firewall-start"},
		{Tag: "keenetic", Path: "router/opt/etc/ndm/netfilter.d/50-vpn-director.sh"},
	}
	got := fileEntries(entries, "merlin")
	want := []FileEntry{
		{Src: "opt/vpn-director/lib/common.sh", Dst: "/opt/vpn-director/lib/common.sh", Exec: true},
		{Src: "opt/vpn-director/vpn-director.json.template", Dst: "/opt/vpn-director/vpn-director.json.template", Exec: false},
		{Src: "jffs/scripts/firewall-start", Dst: "/jffs/scripts/firewall-start", Exec: true},
	}
	if len(got) != len(want) {
		t.Fatalf("fileEntries() = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
