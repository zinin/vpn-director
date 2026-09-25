package updater

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestGetLatestRelease(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify headers
		if r.Header.Get("Accept") != "application/vnd.github.v3+json" {
			t.Errorf("Expected Accept header 'application/vnd.github.v3+json', got %q", r.Header.Get("Accept"))
		}
		if r.Header.Get("User-Agent") != "vpn-director-telegram-bot" {
			t.Errorf("Expected User-Agent header 'vpn-director-telegram-bot', got %q", r.Header.Get("User-Agent"))
		}

		// Verify endpoint
		expectedPath := "/repos/zinin/vpn-director/releases/latest"
		if r.URL.Path != expectedPath {
			t.Errorf("Expected path %q, got %q", expectedPath, r.URL.Path)
		}

		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{
			"tag_name": "v1.2.3",
			"assets": [
				{"url": "https://api.github.com/repos/zinin/vpn-director/releases/assets/301", "id": 301,
				 "name": "telegram-bot-arm64", "label": "", "content_type": "application/octet-stream",
				 "state": "uploaded", "size": 8650936, "download_count": 0,
				 "browser_download_url": "https://github.com/zinin/vpn-director/releases/download/v1.2.3/telegram-bot-arm64"},
				{"url": "https://api.github.com/repos/zinin/vpn-director/releases/assets/302", "id": 302,
				 "name": "telegram-bot-arm", "label": "", "content_type": "application/octet-stream",
				 "state": "uploaded", "size": 8978616, "download_count": 0,
				 "browser_download_url": "https://github.com/zinin/vpn-director/releases/download/v1.2.3/telegram-bot-arm"}
			]
		}`))
	}))
	defer server.Close()

	s := NewWithBaseURL(server.URL)
	release, err := s.GetLatestRelease(context.Background())
	if err != nil {
		t.Fatalf("GetLatestRelease() error = %v", err)
	}

	if release.TagName != "v1.2.3" {
		t.Errorf("TagName = %q, want %q", release.TagName, "v1.2.3")
	}
	if len(release.Assets) != 2 {
		t.Fatalf("len(Assets) = %d, want 2", len(release.Assets))
	}
	if release.Assets[0].Name != "telegram-bot-arm64" {
		t.Errorf("Assets[0].Name = %q, want %q", release.Assets[0].Name, "telegram-bot-arm64")
	}
	// The API's own address of the asset, not github.com's: a network can drop
	// github.com while the API answers, and an update needs the API anyway.
	if want := "https://api.github.com/repos/zinin/vpn-director/releases/assets/301"; release.Assets[0].DownloadURL != want {
		t.Errorf("Assets[0].DownloadURL = %q, want %q", release.Assets[0].DownloadURL, want)
	}
	if release.Assets[1].Name != "telegram-bot-arm" {
		t.Errorf("Assets[1].Name = %q, want %q", release.Assets[1].Name, "telegram-bot-arm")
	}
}

func TestGetLatestRelease_IncludesBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := `{
			"tag_name": "v1.0.0",
			"body": "## Changelog\n- Fix bug\n- Add feature",
			"assets": []
		}`
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(response))
	}))
	defer server.Close()

	svc := NewWithBaseURL(server.URL)
	release, err := svc.GetLatestRelease(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := "## Changelog\n- Fix bug\n- Add feature"
	if release.Body != expected {
		t.Errorf("expected body %q, got %q", expected, release.Body)
	}
}

func TestGetLatestRelease_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	s := NewWithBaseURL(server.URL)
	_, err := s.GetLatestRelease(context.Background())
	if err == nil {
		t.Error("Expected error for 404 response")
	}
}

func TestGetLatestRelease_InvalidJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{invalid json`))
	}))
	defer server.Close()

	s := NewWithBaseURL(server.URL)
	_, err := s.GetLatestRelease(context.Background())
	if err == nil {
		t.Error("Expected error for invalid JSON response")
	}
}

func TestGetLatestRelease_EmptyAssets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"tag_name": "v1.0.0", "assets": []}`))
	}))
	defer server.Close()

	s := NewWithBaseURL(server.URL)
	release, err := s.GetLatestRelease(context.Background())
	if err != nil {
		t.Fatalf("GetLatestRelease() error = %v", err)
	}
	if release.TagName != "v1.0.0" {
		t.Errorf("TagName = %q, want %q", release.TagName, "v1.0.0")
	}
	if len(release.Assets) != 0 {
		t.Errorf("len(Assets) = %d, want 0", len(release.Assets))
	}
}

func TestShouldUpdate(t *testing.T) {
	s := New()

	tests := []struct {
		name           string
		currentVersion string
		latestTag      string
		want           bool
		wantErr        bool
	}{
		{
			name:           "older version should update",
			currentVersion: "v1.0.0",
			latestTag:      "v1.1.0",
			want:           true,
		},
		{
			name:           "same version should not update",
			currentVersion: "v1.1.0",
			latestTag:      "v1.1.0",
			want:           false,
		},
		{
			name:           "newer version should not update",
			currentVersion: "v1.2.0",
			latestTag:      "v1.1.0",
			want:           false,
		},
		{
			name:           "patch update",
			currentVersion: "v1.0.0",
			latestTag:      "v1.0.1",
			want:           true,
		},
		{
			name:           "major update",
			currentVersion: "v1.9.9",
			latestTag:      "v2.0.0",
			want:           true,
		},
		{
			name:           "without v prefix - older",
			currentVersion: "1.0.0",
			latestTag:      "v1.1.0",
			want:           true,
		},
		{
			name:           "without v prefix - same",
			currentVersion: "1.1.0",
			latestTag:      "1.1.0",
			want:           false,
		},
		{
			name:           "dev version should error",
			currentVersion: "dev",
			latestTag:      "v1.1.0",
			wantErr:        true,
		},
		{
			name:           "pre-release current version should error",
			currentVersion: "v1.2.3-rc1",
			latestTag:      "v1.2.4",
			wantErr:        true,
		},
		{
			name:           "pre-release latest tag should error",
			currentVersion: "v1.0.0",
			latestTag:      "v1.1.0-beta",
			wantErr:        true,
		},
		{
			name:           "invalid current version",
			currentVersion: "invalid",
			latestTag:      "v1.0.0",
			wantErr:        true,
		},
		{
			name:           "invalid latest tag",
			currentVersion: "v1.0.0",
			latestTag:      "invalid",
			wantErr:        true,
		},
		{
			name:           "empty current version",
			currentVersion: "",
			latestTag:      "v1.0.0",
			wantErr:        true,
		},
		{
			name:           "empty latest tag",
			currentVersion: "v1.0.0",
			latestTag:      "",
			wantErr:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.ShouldUpdate(tt.currentVersion, tt.latestTag)
			if (err != nil) != tt.wantErr {
				t.Errorf("ShouldUpdate() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("ShouldUpdate(%q, %q) = %v, want %v",
					tt.currentVersion, tt.latestTag, got, tt.want)
			}
		})
	}
}

func TestGetLatestRelease_ContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Slow response - wait for context to be cancelled
		time.Sleep(5 * time.Second)
		w.Write([]byte(`{"tag_name": "v1.0.0", "assets": []}`))
	}))
	defer server.Close()

	s := NewWithBaseURL(server.URL)

	// Cancel context after short delay
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := s.GetLatestRelease(ctx)
	if err == nil {
		t.Error("GetLatestRelease() should fail when context is cancelled")
	}
}

func TestGetLatestRelease_Timeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Slow response exceeding the per-request timeout
		time.Sleep(5 * time.Second)
		w.Write([]byte(`{"tag_name": "v1.0.0", "assets": []}`))
	}))
	defer server.Close()

	s := NewWithBaseURL(server.URL)

	// Use a short parent context timeout to test timeout behavior
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err := s.GetLatestRelease(ctx)
	if err == nil {
		t.Error("GetLatestRelease() should fail when request times out")
	}
}

// TestRepoName_IsRenamed pins the release location: the repository was
// renamed from asuswrt-merlin-vpn-director, and an updater still pointing at
// the old name would only keep working while GitHub's redirect lasts.
func TestRepoName_IsRenamed(t *testing.T) {
	if repoName != "vpn-director" {
		t.Fatalf("repoName = %q, want vpn-director", repoName)
	}
}

func TestGetReleaseByTag(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if want := "/repos/zinin/vpn-director/releases/tags/v1.2.4"; r.URL.Path != want {
			t.Errorf("path = %q, want %q", r.URL.Path, want)
		}
		if got := r.Header.Get("Accept"); got != "application/vnd.github.v3+json" {
			t.Errorf("Accept = %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != "vpn-director-telegram-bot" {
			t.Errorf("User-Agent = %q", got)
		}
		w.Write([]byte(`{"tag_name": "v1.2.4", "body": "notes", "assets": [
			{"url": "https://api.github.com/repos/zinin/vpn-director/releases/assets/302", "id": 302,
			 "name": "webui-arm64", "label": "", "content_type": "application/octet-stream",
			 "state": "uploaded", "size": 8913080, "download_count": 0,
			 "browser_download_url": "https://github.com/zinin/vpn-director/releases/download/v1.2.4/webui-arm64"}]}`))
	}))
	defer server.Close()

	release, err := NewWithBaseURL(server.URL).GetReleaseByTag(context.Background(), "v1.2.4")
	if err != nil {
		t.Fatalf("GetReleaseByTag() error = %v", err)
	}
	if release.TagName != "v1.2.4" || release.Body != "notes" {
		t.Errorf("release = %+v", release)
	}
	if len(release.Assets) != 1 || release.Assets[0].Name != "webui-arm64" ||
		release.Assets[0].DownloadURL != "https://api.github.com/repos/zinin/vpn-director/releases/assets/302" {
		t.Errorf("Assets = %+v", release.Assets)
	}
}

// fakeAssets are the assets of the fake GitHub's release by id.
var fakeAssets = map[string]string{"301": "telegram-bot-arm64", "302": "webui-arm64"}

// fakeGitHub answers for release v1.2.4 the way GitHub does, over TLS. Each
// asset has both of its addresses. The API's answers a request that asks for
// application/octet-stream with a redirect to the file on the CDN, and any
// other with the asset's description in JSON. github.com's does not answer, as
// on the router whose provider dropped github.com while the API still
// answered. The requests that reached github.com are recorded.
type fakeGitHub struct {
	server *httptest.Server

	mu        sync.Mutex
	githubCom []string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{}
	f.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case p == "/repos/zinin/vpn-director/releases/latest", p == "/repos/zinin/vpn-director/releases/tags/v1.2.4":
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			fmt.Fprintf(w, `{"tag_name": "v1.2.4", "body": "notes", "assets": [%s, %s]}`, f.asset("301"), f.asset("302"))
		case strings.HasPrefix(p, "/repos/zinin/vpn-director/releases/assets/"):
			id := strings.TrimPrefix(p, "/repos/zinin/vpn-director/releases/assets/")
			name, ok := fakeAssets[id]
			switch {
			case !ok:
				http.NotFound(w, r)
			case r.Header.Get("Accept") == "application/octet-stream":
				http.Redirect(w, r, "/github-production-release-asset/"+name, http.StatusFound)
			default:
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				io.WriteString(w, f.asset(id))
			}
		case strings.HasPrefix(p, "/github-production-release-asset/"):
			w.Header().Set("Content-Type", "application/octet-stream")
			io.WriteString(w, "the binary "+strings.TrimPrefix(p, "/github-production-release-asset/"))
		case strings.HasPrefix(p, "/zinin/vpn-director/releases/download/"):
			f.mu.Lock()
			f.githubCom = append(f.githubCom, p)
			f.mu.Unlock()
			http.Error(w, "github.com does not answer", http.StatusGatewayTimeout)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

// asset is the API's description of the asset id, as GitHub words it.
func (f *fakeGitHub) asset(id string) string {
	name := fakeAssets[id]
	return fmt.Sprintf(`{"url": "%[1]s/repos/zinin/vpn-director/releases/assets/%[2]s", "id": %[2]s,
		"name": "%[3]s", "label": "", "content_type": "application/octet-stream", "state": "uploaded",
		"size": %[4]d, "download_count": 0,
		"browser_download_url": "%[1]s/zinin/vpn-director/releases/download/v1.2.4/%[3]s"}`,
		f.server.URL, id, name, len("the binary "+name))
}

func (f *fakeGitHub) githubComRequests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.githubCom...)
}

// service is a Service of daemon over the fake GitHub, its update directory dir.
func (f *fakeGitHub) service(daemon, dir string) *Service {
	return &Service{
		httpClient: f.server.Client(),
		baseURL:    f.server.URL,
		updateDir:  dir,
		archSuffix: "arm64",
		daemon:     daemon,
	}
}

// Step 1 fetches its installer through the API. On a network that dropped
// github.com while the API answered, the bot learned of the release and then
// failed to download it: "dial tcp 140.82.121.4:443: connect: connection timed
// out", three times in a row.
func TestDownloadInstaller_FetchesThroughTheAPINotGitHubCom(t *testing.T) {
	f := newFakeGitHub(t)
	dir := t.TempDir()
	s := f.service(DaemonBot, dir)
	release, err := s.GetLatestRelease(context.Background())
	if err != nil {
		t.Fatalf("GetLatestRelease() error = %v", err)
	}

	installer := filepath.Join(dir, "installer")
	if err := s.downloadInstaller(context.Background(), release, installer); err != nil {
		t.Fatalf("downloadInstaller() error = %v", err)
	}
	if data, err := os.ReadFile(installer); err != nil || string(data) != "the binary telegram-bot-arm64" {
		t.Errorf("installer = %q (%v), want the binary telegram-bot-arm64", data, err)
	}
	if got := f.githubComRequests(); len(got) != 0 {
		t.Errorf("github.com was asked for %v", got)
	}
}

// Step 2 fetches the binaries it does not carry itself the same way.
func TestDownloadBinaries_FetchesThroughTheAPINotGitHubCom(t *testing.T) {
	f := newFakeGitHub(t)
	dir := t.TempDir()
	s := f.service(DaemonWebUI, dir)
	s.selfBinary = filepath.Join(dir, "installer")
	if err := os.WriteFile(s.selfBinary, []byte("the new webui"), 0755); err != nil {
		t.Fatal(err)
	}
	release, err := s.GetReleaseByTag(context.Background(), "v1.2.4")
	if err != nil {
		t.Fatalf("GetReleaseByTag() error = %v", err)
	}

	if err := s.downloadBinaries(context.Background(), release); err != nil {
		t.Fatalf("downloadBinaries() error = %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "files", DaemonBot)); err != nil || string(data) != "the binary telegram-bot-arm64" {
		t.Errorf("files/%s = %q (%v), want the binary telegram-bot-arm64", DaemonBot, data, err)
	}
	if got := f.githubComRequests(); len(got) != 0 {
		t.Errorf("github.com was asked for %v", got)
	}
}

func TestGetReleaseByTag_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	if _, err := NewWithBaseURL(server.URL).GetReleaseByTag(context.Background(), "v9.9.9"); err == nil {
		t.Fatal("GetReleaseByTag() of a tag GitHub does not know succeeded")
	}
}
