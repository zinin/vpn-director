package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	neturl "net/url"
	"time"
)

// GitHub API constants.
const (
	repoOwner            = "zinin"
	repoName             = "vpn-director"
	defaultAPIURL        = "https://api.github.com"
	releasesEndpoint     = "/repos/%s/%s/releases/latest"
	releaseByTagEndpoint = "/repos/%s/%s/releases/tags/%s"

	// APITimeout bounds one GitHub API call. Handlers that make such a call
	// inside an HTTP request size their response deadline from it.
	APITimeout = 30 * time.Second
)

// githubRelease represents the GitHub API response for one release.
type githubRelease struct {
	TagName string        `json:"tag_name"`
	Body    string        `json:"body"`
	Assets  []githubAsset `json:"assets"`
}

// githubAsset represents a release asset in the GitHub API response.
type githubAsset struct {
	Name string `json:"name"`
	// URL is the asset's own address on the API, which sends the file, by a
	// redirect to GitHub's CDN, to a request that asks for it. Not
	// browser_download_url: that one is on github.com, and a network can drop
	// github.com while the API, which an update needs anyway, answers.
	URL string `json:"url"`
}

// GetLatestRelease fetches the latest release info from GitHub API.
func (s *Service) GetLatestRelease(ctx context.Context) (*Release, error) {
	return s.fetchRelease(ctx, fmt.Sprintf(releasesEndpoint, repoOwner, repoName))
}

// GetReleaseByTag fetches the release published under tag. Step 2 of a
// self-update installs the release it belongs to, which need not be the
// latest one any more.
func (s *Service) GetReleaseByTag(ctx context.Context, tag string) (*Release, error) {
	return s.fetchRelease(ctx, fmt.Sprintf(releaseByTagEndpoint, repoOwner, repoName, neturl.PathEscape(tag)))
}

// fetchRelease reads one release document from the GitHub API.
func (s *Service) fetchRelease(ctx context.Context, endpoint string) (*Release, error) {
	ctx, cancel := context.WithTimeout(ctx, APITimeout)
	defer cancel()

	baseURL := s.baseURL
	if baseURL == "" {
		baseURL = defaultAPIURL
	}
	url := baseURL + endpoint

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "vpn-director-telegram-bot")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch release: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned status %d", resp.StatusCode)
	}

	var ghRelease githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&ghRelease); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	release := &Release{
		TagName: ghRelease.TagName,
		Body:    ghRelease.Body,
		Assets:  make([]Asset, len(ghRelease.Assets)),
	}
	for i, a := range ghRelease.Assets {
		release.Assets[i] = Asset{
			Name:        a.Name,
			DownloadURL: a.URL,
		}
	}

	return release, nil
}

// ShouldUpdate checks if currentVersion is older than latestTag.
// Returns an error if either version can't be parsed (dev handled by caller).
func (s *Service) ShouldUpdate(currentVersion, latestTag string) (bool, error) {
	current, err := ParseVersion(currentVersion)
	if err != nil {
		return false, fmt.Errorf("parse current version %q: %w", currentVersion, err)
	}

	latest, err := ParseVersion(latestTag)
	if err != nil {
		return false, fmt.Errorf("parse latest version %q: %w", latestTag, err)
	}

	return current.IsOlderThan(latest), nil
}
