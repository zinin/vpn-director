package updater

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/platform"
)

func TestDownloadFile_Success(t *testing.T) {
	content := "test file content"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(content))
	}))
	defer server.Close()

	tempDir := t.TempDir()
	target := filepath.Join(tempDir, "subdir", "file.txt")
	s := New()

	err := s.downloadFile(context.Background(), server.URL, target)
	if err != nil {
		t.Fatalf("downloadFile() error = %v", err)
	}

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("Failed to read downloaded file: %v", err)
	}
	if string(data) != content {
		t.Errorf("File content = %q, want %q", string(data), content)
	}
}

// The API answers an asset's address with the asset's description in JSON
// unless the request asks for the file. A description that comes back all the
// same - whatever lost the header on the way - is no daemon binary: the update
// script would install it, and the daemon would not start again.
func TestAssetDownload_RefusesTheAssetsDescription(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Write([]byte(`{"url": "https://api.github.com/repos/zinin/vpn-director/releases/assets/301", "id": 301,
			"name": "telegram-bot-arm64", "label": "", "content_type": "application/octet-stream",
			"state": "uploaded", "size": 8650936, "download_count": 0,
			"browser_download_url": "https://github.com/zinin/vpn-director/releases/download/v1.2.4/telegram-bot-arm64"}`))
	}))
	defer server.Close()
	release := &Release{TagName: "v1.2.4"}
	for i, d := range Daemons {
		release.Assets = append(release.Assets, Asset{
			Name:        d.Name + "-arm64",
			DownloadURL: server.URL + "/repos/zinin/vpn-director/releases/assets/" + strconv.Itoa(301+i),
		})
	}

	t.Run("the installer of step 1", func(t *testing.T) {
		dir := t.TempDir()
		s := &Service{httpClient: server.Client(), updateDir: dir, archSuffix: "arm64", daemon: DaemonBot}
		installer := filepath.Join(dir, "installer")
		if err := s.downloadInstaller(context.Background(), release, installer); err == nil {
			t.Fatal("downloadInstaller() took the asset's description for the installer")
		}
		if _, err := os.Stat(installer); !os.IsNotExist(err) {
			t.Errorf("an installer is left behind (%v)", err)
		}
	})
	t.Run("the binaries of step 2", func(t *testing.T) {
		dir := t.TempDir()
		s := &Service{httpClient: server.Client(), updateDir: dir, archSuffix: "arm64"}
		if err := s.downloadBinaries(context.Background(), release); err == nil {
			t.Fatal("downloadBinaries() took the asset's description for a binary")
		}
		for _, d := range Daemons {
			if _, err := os.Stat(filepath.Join(dir, "files", d.Name)); !os.IsNotExist(err) {
				t.Errorf("files/%s is there (%v)", d.Name, err)
			}
		}
	})
}

func TestDownloadFile_CreatesParentDirs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("content"))
	}))
	defer server.Close()

	tempDir := t.TempDir()
	target := filepath.Join(tempDir, "a", "b", "c", "file.txt")
	s := New()

	err := s.downloadFile(context.Background(), server.URL, target)
	if err != nil {
		t.Fatalf("downloadFile() error = %v", err)
	}

	if _, err := os.Stat(target); err != nil {
		t.Errorf("Target file not created: %v", err)
	}
}

func TestDownloadFile_HTTPError(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
	}{
		{"NotFound", http.StatusNotFound},
		{"InternalServerError", http.StatusInternalServerError},
		{"Forbidden", http.StatusForbidden},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
			}))
			defer server.Close()

			tempDir := t.TempDir()
			s := New()

			err := s.downloadFile(context.Background(), server.URL, filepath.Join(tempDir, "file"))
			if err == nil {
				t.Errorf("downloadFile() should fail for HTTP %d", tt.statusCode)
			}
		})
	}
}

func TestDownloadFile_SizeLimit(t *testing.T) {
	// Create content larger than maxFileSize (50MB)
	// We can't actually allocate 50MB in tests, so we simulate with a slow reader
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// Write exactly maxFileSize bytes
		chunk := strings.Repeat("x", 1024)
		for i := 0; i < maxFileSize/1024; i++ {
			w.Write([]byte(chunk))
		}
		// Write one more byte to exceed limit
		w.Write([]byte("!"))
	}))
	defer server.Close()

	tempDir := t.TempDir()
	target := filepath.Join(tempDir, "large_file")
	s := New()

	err := s.downloadFile(context.Background(), server.URL, target)
	if err == nil {
		t.Error("downloadFile() should fail for file exceeding size limit")
	}
	if !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Errorf("Error should mention size limit, got: %v", err)
	}

	// File should be removed on size limit error
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Error("File should be removed after size limit exceeded")
	}
}

func TestDownloadFile_ContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Slow response - wait for context to be cancelled
		time.Sleep(5 * time.Second)
		w.Write([]byte("content"))
	}))
	defer server.Close()

	tempDir := t.TempDir()
	s := New()

	// Cancel context after short delay
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := s.downloadFile(ctx, server.URL, filepath.Join(tempDir, "file"))
	if err == nil {
		t.Error("downloadFile() should fail when context is cancelled")
	}
}

func TestDownloadFile_UserAgent(t *testing.T) {
	var gotUserAgent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUserAgent = r.Header.Get("User-Agent")
		w.Write([]byte("ok"))
	}))
	defer server.Close()

	tempDir := t.TempDir()
	s := New()

	s.downloadFile(context.Background(), server.URL, filepath.Join(tempDir, "file"))

	if gotUserAgent != "vpn-director-telegram-bot" {
		t.Errorf("User-Agent = %q, want %q", gotUserAgent, "vpn-director-telegram-bot")
	}
}

func TestDownloadRelease_CleansBeforeDownload(t *testing.T) {
	// Server that responds to all requests
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("content"))
	}))
	defer server.Close()

	tempDir := t.TempDir()
	filesDir := filepath.Join(tempDir, "files")

	// Create pre-existing file
	if err := os.MkdirAll(filesDir, 0755); err != nil {
		t.Fatalf("Failed to create files dir: %v", err)
	}
	oldFile := filepath.Join(filesDir, "old_file.txt")
	if err := os.WriteFile(oldFile, []byte("old content"), 0644); err != nil {
		t.Fatalf("Failed to create old file: %v", err)
	}

	s := &Service{
		httpClient: &http.Client{},
		baseURL:    server.URL,
		rawBaseURL: server.URL,
		updateDir:  tempDir,
	}

	// This will fail because server doesn't serve proper paths,
	// but it will clean the directory first
	_ = s.DownloadRelease(context.Background(), &Release{TagName: "v1.0.0"})

	// Old file should be gone (directory was cleaned)
	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Error("DownloadRelease() should clean files directory before download")
	}
}

const testManifestBody = "common router/opt/vpn-director/vpn-director.sh\n" +
	"common router/opt/vpn-director/lib/common.sh\n" +
	"merlin router/jffs/scripts/firewall-start\n" +
	"keenetic router/opt/etc/ndm/netfilter.d/50-vpn-director.sh\n"

// TestDownloadRelease_FetchesManifestThenPlatformFiles drives the whole
// download through a server the test controls: the manifest comes first, then
// every common and merlin file in manifest order, and nothing tagged keenetic.
func TestDownloadRelease_FetchesManifestThenPlatformFiles(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/"+manifestPath) {
			w.Write([]byte(testManifestBody))
			return
		}
		w.Write([]byte("content"))
	}))
	defer server.Close()

	tempDir := t.TempDir()
	s := &Service{
		httpClient: &http.Client{},
		baseURL:    server.URL,
		rawBaseURL: server.URL,
		updateDir:  tempDir,
		archSuffix: "arm64",
		platform:   "merlin",
	}

	// No assets in the release, so downloadBinaries fails after the files.
	_ = s.DownloadRelease(context.Background(), &Release{TagName: "v1.0.0"})

	mu.Lock()
	defer mu.Unlock()
	prefix := "/" + repoOwner + "/" + repoName + "/refs/tags/v1.0.0/"
	want := []string{
		prefix + manifestPath,
		prefix + "router/opt/vpn-director/vpn-director.sh",
		prefix + "router/opt/vpn-director/lib/common.sh",
		prefix + "router/jffs/scripts/firewall-start",
	}
	if strings.Join(paths, "\n") != strings.Join(want, "\n") {
		t.Fatalf("request paths:\n%s\nwant:\n%s", strings.Join(paths, "\n"), strings.Join(want, "\n"))
	}
	for _, f := range []string{"files/files.manifest", "files/opt/vpn-director/lib/common.sh", "files/jffs/scripts/firewall-start"} {
		if _, err := os.Stat(filepath.Join(tempDir, f)); err != nil {
			t.Errorf("%s not written: %v", f, err)
		}
	}
}

func TestDownloadRelease_FailsWithoutManifest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	s := &Service{
		httpClient: &http.Client{},
		baseURL:    server.URL,
		rawBaseURL: server.URL,
		updateDir:  t.TempDir(),
		archSuffix: "arm64",
	}
	err := s.DownloadRelease(context.Background(), &Release{TagName: "v1.0.0"})
	if err == nil || !strings.Contains(err.Error(), "files.manifest") {
		t.Fatalf("DownloadRelease() error = %v, want one naming files.manifest", err)
	}
}

// TestDownloadRelease_RefusesAManifestWithNoFileForThePlatform closes the one
// hole that a zero-entry selection would turn into a silent success: the
// binaries get swapped while every script and init file stays at the old
// version. downloadFile has no minimum-size check, so a zero-byte HTTP 200 is
// a manifest as far as the parser is concerned. The releases below carry
// working assets over TLS on purpose - without the guard downloadBinaries
// fetches them both and DownloadRelease returns nil.
func TestDownloadRelease_RefusesAManifestWithNoFileForThePlatform(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
	}{
		{name: "empty", body: ""},
		{name: "comments only", body: "# VPN Director file manifest\n\n# nothing here yet\n"},
		{name: "other platforms only", body: "keenetic router/opt/etc/ndm/netfilter.d/50-vpn-director.sh\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			var paths []string
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				paths = append(paths, r.URL.Path)
				mu.Unlock()
				if strings.HasSuffix(r.URL.Path, "/"+manifestPath) {
					w.Write([]byte(tt.body))
					return
				}
				w.Write([]byte("content"))
			}))
			defer server.Close()

			tempDir := t.TempDir()
			s := &Service{
				httpClient: server.Client(),
				baseURL:    server.URL,
				rawBaseURL: server.URL,
				updateDir:  tempDir,
				archSuffix: "arm64",
				platform:   "merlin",
			}

			release := &Release{TagName: "v1.0.0"}
			for _, d := range Daemons {
				release.Assets = append(release.Assets, Asset{
					Name:        d.Name + "-arm64",
					DownloadURL: server.URL + "/" + d.Name + "-arm64",
				})
			}

			err := s.DownloadRelease(context.Background(), release)
			if err == nil {
				t.Fatal("DownloadRelease() accepted a manifest that lists no file for this platform")
			}
			if !strings.Contains(err.Error(), "merlin") {
				t.Errorf("error = %v, want it to name the platform", err)
			}
			for _, d := range Daemons {
				if _, err := os.Stat(filepath.Join(tempDir, "files", d.Name)); !os.IsNotExist(err) {
					t.Errorf("binary %s was downloaded on top of scripts that were not", d.Name)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if len(paths) != 1 || !strings.HasSuffix(paths[0], "/"+manifestPath) {
				t.Errorf("request paths = %v, want the manifest and nothing else", paths)
			}
		})
	}
}

func TestGetPlatform(t *testing.T) {
	if got, err := (&Service{platform: "keenetic"}).getPlatform(); err != nil || got != "keenetic" {
		t.Errorf("injected: getPlatform() = %q, %v; want keenetic", got, err)
	}
	t.Setenv(platform.EnvVar, platform.Keenetic)
	if got, err := (&Service{}).getPlatform(); err != nil || got != "keenetic" {
		t.Errorf("detected: getPlatform() = %q, %v; want keenetic from %s", got, err, platform.EnvVar)
	}
	s := &Service{}
	s.SetPlatform("merlin")
	if got, _ := s.getPlatform(); got != "merlin" {
		t.Errorf("SetPlatform: getPlatform() = %q, want merlin", got)
	}
	t.Setenv(platform.EnvVar, "")
	t.Setenv("VPD_PROBE_ROOT", t.TempDir())
	if _, err := (&Service{}).getPlatform(); err == nil {
		t.Error("undetectable: getPlatform() guessed a platform instead of failing")
	}
}

func TestDownloadRelease_RefusesWhenThePlatformIsUnknown(t *testing.T) {
	t.Setenv(platform.EnvVar, "")
	t.Setenv("VPD_PROBE_ROOT", t.TempDir())
	s := &Service{updateDir: t.TempDir()}
	err := s.DownloadRelease(context.Background(), &Release{TagName: "v1.0.0"})
	if err == nil || !strings.Contains(err.Error(), "unsupported platform") {
		t.Fatalf("DownloadRelease() = %v, want the detection error", err)
	}
}

func TestDownloadScriptFile_PathTransformation(t *testing.T) {
	// This test verifies the path transformation logic:
	// "router/opt/vpn-director/lib/common.sh" → "files/opt/vpn-director/lib/common.sh"
	// The downloadScriptFile uses raw.githubusercontent.com URL format, not GitHub API.

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// downloadScriptFile builds its URL from repoRawPath, which contains the file path
		if strings.Contains(r.URL.Path, "common.sh") {
			w.Write([]byte("#!/bin/bash\necho test"))
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	tempDir := t.TempDir()

	// We need to test the path transformation directly since downloadScriptFile
	// uses a hardcoded URL format. Let's test the target path calculation.
	file := "router/opt/vpn-director/lib/common.sh"
	expectedTarget := filepath.Join(tempDir, "files", "opt/vpn-director/lib/common.sh")
	actualTarget := filepath.Join(tempDir, "files", strings.TrimPrefix(file, "router"))

	if actualTarget != expectedTarget {
		t.Errorf("Target path = %q, want %q", actualTarget, expectedTarget)
	}

	// Verify TrimPrefix works correctly for various paths
	testCases := []struct {
		input    string
		expected string
	}{
		{"router/opt/vpn-director/vpn-director.sh", "/opt/vpn-director/vpn-director.sh"},
		{"router/jffs/scripts/firewall-start", "/jffs/scripts/firewall-start"},
		{"router/opt/etc/init.d/S99vpn-director", "/opt/etc/init.d/S99vpn-director"},
	}

	for _, tc := range testCases {
		result := strings.TrimPrefix(tc.input, "router")
		if result != tc.expected {
			t.Errorf("TrimPrefix(%q, \"router\") = %q, want %q", tc.input, result, tc.expected)
		}
	}
}

func TestArchAssetSuffix(t *testing.T) {
	tests := []struct {
		goarch  string
		want    string
		wantErr bool
	}{
		{goarch: "arm64", want: "arm64"},
		{goarch: "arm", want: "arm"},
		{goarch: "mipsle", want: "mipsle"},
		{goarch: "mips", wantErr: true},
		{goarch: "amd64", wantErr: true},
		{goarch: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.goarch, func(t *testing.T) {
			got, err := archAssetSuffix(tt.goarch)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("archAssetSuffix(%q) = %q, want error", tt.goarch, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("archAssetSuffix(%q) error = %v", tt.goarch, err)
			}
			if got != tt.want {
				t.Errorf("archAssetSuffix(%q) = %q, want %q", tt.goarch, got, tt.want)
			}
		})
	}
}

// TestGetArchSuffix_ProductionBranchMatchesRuntime pins the one line no CI
// machine executes on the target architecture: a typo there breaks self-update
// for every router at once. Comparing error strings and not merely "both
// failed" is the point - on an amd64 box both branches error either way, so a
// weaker assertion would pass even if the production branch asked about
// runtime.GOOS.
func TestGetArchSuffix_ProductionBranchMatchesRuntime(t *testing.T) {
	got, gotErr := (&Service{}).getArchSuffix()
	want, wantErr := archAssetSuffix(runtime.GOARCH)

	if got != want {
		t.Errorf("getArchSuffix() = %q, want %q", got, want)
	}
	switch {
	case gotErr == nil && wantErr == nil:
	case gotErr == nil || wantErr == nil:
		t.Fatalf("getArchSuffix() error = %v, archAssetSuffix error = %v", gotErr, wantErr)
	case gotErr.Error() != wantErr.Error():
		t.Errorf("getArchSuffix() error = %q, want %q", gotErr, wantErr)
	}
}

func TestDownloadBinaries_DownloadsEveryDaemon(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("binary of " + strings.TrimPrefix(r.URL.Path, "/")))
	}))
	defer server.Close()

	tempDir := t.TempDir()
	s := &Service{httpClient: server.Client(), updateDir: tempDir, archSuffix: "arm64"}

	release := &Release{TagName: "v1.0.0"}
	for _, d := range Daemons {
		release.Assets = append(release.Assets, Asset{
			Name:        d.Name + "-arm64",
			DownloadURL: server.URL + "/" + d.Name + "-arm64",
		})
	}

	if err := s.downloadBinaries(context.Background(), release); err != nil {
		t.Fatalf("downloadBinaries() error = %v", err)
	}

	for _, d := range Daemons {
		target := filepath.Join(tempDir, "files", d.Name)
		data, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("binary for %s not downloaded: %v", d.Name, err)
		}
		if want := "binary of " + d.Name + "-arm64"; string(data) != want {
			t.Errorf("%s content = %q, want %q", d.Name, data, want)
		}
	}
}

func TestDownloadBinaries_MissingAssetIsAnError(t *testing.T) {
	// A release that ships only one of the two binaries would install a new
	// bot next to an old Web UI; refuse the whole download instead.
	// The present assets must be served for real, not stubbed with an
	// unroutable URL: downloadBinaries downloads inside the loop, so a stub
	// makes the subtest turn on daemon ordering and DNS behaviour — it fails
	// on the first daemon it fetches instead of on the missing asset.
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("binary"))
	}))
	defer server.Close()

	for _, missing := range Daemons {
		t.Run("without "+missing.Name, func(t *testing.T) {
			tempDir := t.TempDir()
			s := &Service{httpClient: server.Client(), updateDir: tempDir, archSuffix: "arm64"}

			release := &Release{TagName: "v1.0.0"}
			for _, d := range Daemons {
				if d.Name == missing.Name {
					continue
				}
				release.Assets = append(release.Assets, Asset{
					Name:        d.Name + "-arm64",
					DownloadURL: server.URL + "/" + d.Name + "-arm64",
				})
			}

			err := s.downloadBinaries(context.Background(), release)
			if err == nil {
				t.Fatalf("downloadBinaries() must fail without asset %s-arm64", missing.Name)
			}
			if !strings.Contains(err.Error(), missing.Name+"-arm64") {
				t.Errorf("error %q should name the missing asset %s-arm64", err, missing.Name)
			}
		})
	}
}

// TestDownloadBinaries_RefusesAPlainHTTPAsset closes a plaintext-downgrade path
// on a file that is written to /opt and executed as root. GitHub always answers
// with https, so this is defence in depth against a tampered API response.
func TestDownloadBinaries_RefusesAPlainHTTPAsset(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("binary"))
	}))
	defer server.Close()

	s := &Service{httpClient: &http.Client{}, updateDir: t.TempDir(), archSuffix: "arm64"}

	release := &Release{TagName: "v1.0.0"}
	for _, d := range Daemons {
		release.Assets = append(release.Assets, Asset{
			Name:        d.Name + "-arm64",
			DownloadURL: server.URL + "/" + d.Name + "-arm64", // http://
		})
	}

	err := s.downloadBinaries(context.Background(), release)
	if err == nil {
		t.Fatal("downloadBinaries() accepted a plain-http asset URL")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Errorf("error = %v, want it to name the scheme requirement", err)
	}
}

// Step 2 of a self-update is the new release's binary of its own daemon, so
// the payload takes that binary from the running executable instead of
// fetching the same bytes again: a hard link, which costs no tmpfs.
func TestDownloadBinaries_TakesItsOwnBinaryFromTheRunningExecutable(t *testing.T) {
	var mu sync.Mutex
	var requested []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requested = append(requested, r.URL.Path)
		mu.Unlock()
		w.Write([]byte("binary of " + strings.TrimPrefix(r.URL.Path, "/")))
	}))
	defer server.Close()

	tempDir := t.TempDir()
	self := filepath.Join(tempDir, "installer")
	if err := os.WriteFile(self, []byte("the running webui"), 0755); err != nil {
		t.Fatal(err)
	}
	s := &Service{httpClient: server.Client(), updateDir: tempDir, archSuffix: "arm64",
		daemon: DaemonWebUI, selfBinary: self}

	release := &Release{TagName: "v1.0.0"}
	for _, d := range Daemons {
		release.Assets = append(release.Assets, Asset{
			Name:        d.Name + "-arm64",
			DownloadURL: server.URL + "/" + d.Name + "-arm64",
		})
	}

	if err := s.downloadBinaries(context.Background(), release); err != nil {
		t.Fatalf("downloadBinaries() error = %v", err)
	}

	selfInfo, err := os.Stat(self)
	if err != nil {
		t.Fatal(err)
	}
	ownInfo, err := os.Stat(filepath.Join(tempDir, "files", DaemonWebUI))
	if err != nil {
		t.Fatalf("no payload binary for %s: %v", DaemonWebUI, err)
	}
	if !os.SameFile(selfInfo, ownInfo) {
		t.Errorf("files/%s is not a hard link to the running executable", DaemonWebUI)
	}
	data, err := os.ReadFile(filepath.Join(tempDir, "files", DaemonBot))
	if err != nil || string(data) != "binary of "+DaemonBot+"-arm64" {
		t.Errorf("files/%s = %q (%v), want the downloaded asset", DaemonBot, data, err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, p := range requested {
		if strings.Contains(p, DaemonWebUI) {
			t.Errorf("downloaded %s although the running executable is that binary", p)
		}
	}
}

// A hard link needs one filesystem; without one the binary is copied, and it
// has to arrive executable.
func TestCopyExecutable_CopiesContentAndTheExecutableBit(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("binary"), 0644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst")

	if err := copyExecutable(src, dst); err != nil {
		t.Fatalf("copyExecutable() error = %v", err)
	}
	data, err := os.ReadFile(dst)
	if err != nil || string(data) != "binary" {
		t.Fatalf("dst = %q (%v), want the source content", data, err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0100 == 0 {
		t.Errorf("dst mode = %v, want it executable", info.Mode().Perm())
	}
}

// files/ survives a failed attempt, so step 2 can find its own binary already
// there: a hard link to the executable it is running as. os.Link then fails
// with "file exists", and a copy onto that destination opens it with O_TRUNC -
// which empties the source along with it, because they are one file. The
// payload binary would be zero bytes and the running daemon gone with it.
func TestLinkOrCopy_KeepsTheSourceWhenTheDestinationIsAlreadyLinkedToIt(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "installer")
	if err := os.WriteFile(src, []byte("the new webui"), 0755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "files", DaemonWebUI)
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(src, dst); err != nil {
		t.Fatal(err)
	}

	if err := linkOrCopy(src, dst); err != nil {
		t.Fatalf("linkOrCopy() error = %v", err)
	}

	for _, path := range []string{src, dst} {
		if data, err := os.ReadFile(path); err != nil || string(data) != "the new webui" {
			t.Errorf("%s = %q (%v), want the binary whole", filepath.Base(path), data, err)
		}
	}
}

// A destination left by an earlier attempt is replaced by the binary asked
// for, and arrives with the mode a daemon needs rather than the one that file
// happened to have.
func TestLinkOrCopy_ReplacesAnExistingDestination(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "installer")
	if err := os.WriteFile(src, []byte("the new webui"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(src, 0755); err != nil { // umask does not decide this
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "files", DaemonWebUI)
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("an older build"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dst, 0644); err != nil {
		t.Fatal(err)
	}

	if err := linkOrCopy(src, dst); err != nil {
		t.Fatalf("linkOrCopy() error = %v", err)
	}

	if data, err := os.ReadFile(dst); err != nil || string(data) != "the new webui" {
		t.Errorf("dst = %q (%v), want the binary that was asked for", data, err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0755 {
		t.Errorf("dst mode = %v, want 0755", info.Mode().Perm())
	}
}

// O_CREATE leaves the mode of a file that is already there alone, so a
// destination that exists has to be made executable explicitly.
func TestCopyExecutable_MakesAnExistingDestinationExecutable(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("binary"), 0755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(dst, []byte("an older build"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dst, 0644); err != nil {
		t.Fatal(err)
	}

	if err := copyExecutable(src, dst); err != nil {
		t.Fatalf("copyExecutable() error = %v", err)
	}

	if data, err := os.ReadFile(dst); err != nil || string(data) != "binary" {
		t.Errorf("dst = %q (%v), want the source content", data, err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0755 {
		t.Errorf("dst mode = %v, want 0755", info.Mode().Perm())
	}
}

// A copy that fails leaves nothing behind under the name of a whole binary:
// the update script would install it and the daemon would not start. The read
// is what fails here; a Close that fails is removed the same way, and no test
// in this suite can bring one about honestly.
func TestCopyExecutable_LeavesNoPartialFileBehind(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.Mkdir(src, 0755); err != nil { // opens, but cannot be read
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst")

	if err := copyExecutable(src, dst); err == nil {
		t.Fatal("copyExecutable() reported a directory copied")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Error("a failed copy left its destination behind")
	}
}

// Step 2 is its own daemon's binary of the release it installs, so that asset
// is one the release need not carry at all: a release that ships only the
// other daemon's still installs.
func TestDownloadBinaries_NeedsNoAssetForTheDaemonItIs(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("binary of " + strings.TrimPrefix(r.URL.Path, "/")))
	}))
	defer server.Close()

	tempDir := t.TempDir()
	self := filepath.Join(tempDir, "installer")
	if err := os.WriteFile(self, []byte("the running webui"), 0755); err != nil {
		t.Fatal(err)
	}
	s := &Service{httpClient: server.Client(), updateDir: tempDir, archSuffix: "arm64",
		daemon: DaemonWebUI, selfBinary: self}
	release := &Release{TagName: "v1.0.0", Assets: []Asset{
		{Name: DaemonBot + "-arm64", DownloadURL: server.URL + "/" + DaemonBot + "-arm64"},
	}}

	if err := s.downloadBinaries(context.Background(), release); err != nil {
		t.Fatalf("downloadBinaries() error = %v, want the missing asset to be the one step 2 is", err)
	}

	selfInfo, err := os.Stat(self)
	if err != nil {
		t.Fatal(err)
	}
	ownInfo, err := os.Stat(filepath.Join(tempDir, "files", DaemonWebUI))
	if err != nil || !os.SameFile(selfInfo, ownInfo) {
		t.Errorf("files/%s is not the running executable (%v)", DaemonWebUI, err)
	}
}

// A hard link needs one filesystem. The installer and files/ share /tmp on a
// router, so the copy is the path nothing exercises there - it needs a second
// filesystem, and /dev/shm is the one a Linux box is likely to have.
func TestLinkOrCopy_CopiesToAnotherFilesystem(t *testing.T) {
	elsewhere, err := os.MkdirTemp("/dev/shm", "vpn-director-test-")
	if err != nil {
		t.Skipf("no second filesystem to copy from: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(elsewhere) })

	dir := t.TempDir()
	src := filepath.Join(elsewhere, "installer")
	if err := os.WriteFile(src, []byte("the new webui"), 0755); err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(dir, "probe")
	if err := os.Link(src, probe); !errors.Is(err, syscall.EXDEV) {
		os.Remove(probe)
		t.Skipf("/dev/shm and %s are one filesystem, so a link would not fall back: %v", dir, err)
	}

	dst := filepath.Join(dir, "files", DaemonWebUI)
	if err := linkOrCopy(src, dst); err != nil {
		t.Fatalf("linkOrCopy() error = %v", err)
	}

	if data, err := os.ReadFile(dst); err != nil || string(data) != "the new webui" {
		t.Errorf("dst = %q (%v), want the source content", data, err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0100 == 0 {
		t.Errorf("dst mode = %v, want it executable", info.Mode().Perm())
	}
}

// The claim was checked before the request but the file was published with
// os.Create only after the round trip, so a step 2 that lost the update
// directory mid-transfer truncated the payload of the retry that took it over.
// telegram-bot.md requires the check before every file step 2 writes.
func TestDownloadScriptFile_LeavesTheTargetAloneWhenTheClaimIsLostMidTransfer(t *testing.T) {
	var mu sync.Mutex
	claimed := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Another daemon takes the update directory over while this body is in
		// flight - the death of step 1's daemon the handover rule describes.
		mu.Lock()
		claimed = false
		mu.Unlock()
		w.Write([]byte("payload of the download that lost the directory"))
	}))
	defer server.Close()

	tempDir := t.TempDir()
	target := filepath.Join(tempDir, "files", "opt/vpn-director/lib/common.sh")
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(target, []byte("the retry's payload"), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	s := &Service{
		httpClient: &http.Client{},
		rawBaseURL: server.URL,
		updateDir:  tempDir,
		checkClaim: func() error {
			mu.Lock()
			defer mu.Unlock()
			if claimed {
				return nil
			}
			return errClaimLost
		},
	}

	err := s.downloadScriptFile(context.Background(), "v1.0.0", "router/opt/vpn-director/lib/common.sh")
	if !errors.Is(err, errClaimLost) {
		t.Fatalf("downloadScriptFile() error = %v, want errClaimLost", err)
	}
	got, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatalf("ReadFile(target) error = %v", readErr)
	}
	if string(got) != "the retry's payload" {
		t.Errorf("target = %q, want the retry's payload untouched", got)
	}
}
