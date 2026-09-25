package updater

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Download constants.
const (
	defaultRawURL   = "https://raw.githubusercontent.com"
	repoRawPath     = "/%s/%s/refs/tags/%s/%s"
	downloadTimeout = 2 * time.Minute
	maxFileSize     = 50 * 1024 * 1024 // 50MB
	// assetMediaType is what a release asset is asked for at its API address.
	assetMediaType = "application/octet-stream"
)

// DownloadRelease downloads the release manifest, then every file it lists
// for this platform, then the daemon binaries. Cleans files/ before starting.
func (s *Service) DownloadRelease(ctx context.Context, release *Release) error {
	tag, err := s.getPlatform()
	if err != nil {
		return err
	}

	filesDir := s.getFilesDir()

	// Before the first and most destructive write of all, as before every
	// one that follows.
	if err := s.requireClaim(); err != nil {
		return err
	}

	// Clean before download to ensure fresh state
	os.RemoveAll(filesDir)

	// Create directory structure
	if err := os.MkdirAll(filesDir, 0755); err != nil {
		return fmt.Errorf("create files directory: %w", err)
	}

	entries, err := s.fetchManifest(ctx, release.TagName)
	if err != nil {
		return err
	}

	// A selection of nothing would swap the daemon binaries and refresh no
	// script and no init file - worse than no update, and silent, because
	// downloadFile accepts a zero-byte HTTP 200 as a manifest. install.sh
	// refuses the same state on its side.
	files := manifestFilesFor(entries, tag)
	if len(files) == 0 {
		return fmt.Errorf("release %s: files.manifest lists no file for platform %s", release.TagName, tag)
	}

	for _, file := range files {
		if err := s.requireClaim(); err != nil {
			return err
		}
		if err := s.downloadScriptFile(ctx, release.TagName, file); err != nil {
			if errors.Is(err, errClaimLost) {
				return err
			}
			return fmt.Errorf("download %s: %w", file, err)
		}
	}

	// Download daemon binaries
	if err := s.downloadBinaries(ctx, release); err != nil {
		if errors.Is(err, errClaimLost) {
			// Not a download failure, and worded the same wherever in the
			// download the claim went: pass it on as it stands.
			return err
		}
		return fmt.Errorf("download binaries: %w", err)
	}

	return nil
}

// fetchManifest downloads router/files.manifest of the release into files/
// (the update script reads it from there) and parses it. A release without a
// manifest cannot be installed.
func (s *Service) fetchManifest(ctx context.Context, tag string) ([]ManifestEntry, error) {
	if err := s.downloadScriptFile(ctx, tag, manifestPath); err != nil {
		if errors.Is(err, errClaimLost) {
			return nil, err
		}
		return nil, fmt.Errorf("release %s has no files.manifest: %w", tag, err)
	}
	f, err := os.Open(filepath.Join(s.getFilesDir(), "files.manifest"))
	if err != nil {
		return nil, fmt.Errorf("open downloaded files.manifest: %w", err)
	}
	defer f.Close()
	return parseManifest(f)
}

// getRawBaseURL returns the host that serves repository files at a tag. Tests
// point it at an httptest server, so the unit suite never reaches GitHub - the
// same seam baseURL provides for the API.
func (s *Service) getRawBaseURL() string {
	if s.rawBaseURL != "" {
		return s.rawBaseURL
	}
	return defaultRawURL
}

func (s *Service) downloadScriptFile(ctx context.Context, tag, file string) error {
	url := s.getRawBaseURL() + fmt.Sprintf(repoRawPath, repoOwner, repoName, tag, file)

	// Target: "router/opt/vpn-director/lib/common.sh" → "files/opt/vpn-director/lib/common.sh"
	target := filepath.Join(s.getFilesDir(), strings.TrimPrefix(file, "router/"))

	return s.downloadFile(ctx, url, target)
}

// archAssetSuffix maps a Go architecture to the release asset suffix.
func archAssetSuffix(goarch string) (string, error) {
	switch goarch {
	case "arm64":
		return "arm64", nil
	case "arm":
		return "arm", nil
	case "mipsle":
		// Keenetic MIPS models, all little-endian; built with GOMIPS=softfloat.
		return "mipsle", nil
	default:
		return "", fmt.Errorf("unsupported architecture: %s", goarch)
	}
}

// getArchSuffix returns the release asset suffix for this build.
func (s *Service) getArchSuffix() (string, error) {
	if s.archSuffix != "" {
		return s.archSuffix, nil
	}
	return archAssetSuffix(runtime.GOARCH)
}

// downloadBinaries puts one binary per daemon into files/<name>, downloaded
// from the release assets - except its own daemon's, which step 2 of a
// self-update already is: with selfBinary set, that one is hard-linked from
// this executable, or copied when a link is not possible.
// A release missing one of the binaries it does download is a download error:
// installing a new bot next to an old Web UI leaves two halves of different
// versions on the router.
func (s *Service) downloadBinaries(ctx context.Context, release *Release) error {
	suffix, err := s.getArchSuffix()
	if err != nil {
		return err
	}

	for _, d := range Daemons {
		if err := s.requireClaim(); err != nil {
			return err
		}
		target := filepath.Join(s.getFilesDir(), d.Name)
		// Step 2 of a self-update is this daemon's binary of the release it
		// installs; fetching that asset again would download the same bytes.
		if s.selfBinary != "" && d.Name == s.daemon {
			if err := linkOrCopy(s.selfBinary, target); err != nil {
				return fmt.Errorf("take %s from %s: %w", d.Name, s.selfBinary, err)
			}
			continue
		}
		assetName := d.Name + "-" + suffix
		url := assetURL(release, assetName)
		if url == "" {
			return fmt.Errorf("asset %s not found in release", assetName)
		}
		if err := requireHTTPS(url); err != nil {
			return fmt.Errorf("asset %s: %w", assetName, err)
		}
		if err := s.downloadAsset(ctx, url, target); err != nil {
			if errors.Is(err, errClaimLost) {
				return err
			}
			return fmt.Errorf("download %s: %w", assetName, err)
		}
	}
	return nil
}

// assetURL returns the download URL of the named asset, or "" when the
// release does not carry it.
func assetURL(release *Release, name string) string {
	for _, a := range release.Assets {
		if a.Name == name {
			return a.DownloadURL
		}
	}
	return ""
}

// requireHTTPS rejects an asset URL that is not https. The URL comes verbatim
// from the GitHub API response and the file it names is written to /opt and
// executed as root, so the one scheme downgrade that would hand a network
// attacker that file is refused here rather than trusted away.
func requireHTTPS(rawURL string) error {
	u, err := neturl.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parse download url: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("refusing a non-https download url (scheme %q)", u.Scheme)
	}
	return nil
}

func (s *Service) downloadFile(ctx context.Context, url, target string) error {
	return s.download(ctx, url, target, false)
}

// downloadAsset downloads a release asset from its address on the API. The API
// sends the file only to a request that asks for assetMediaType, and the
// asset's description in JSON to any other - also to one whose header got lost
// on the way. A description is refused: the update script would install it as
// a daemon that never starts.
func (s *Service) downloadAsset(ctx context.Context, url, target string) error {
	return s.download(ctx, url, target, true)
}

func (s *Service) download(ctx context.Context, url, target string, asset bool) error {
	// Per-request timeout
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()

	// Create parent directory
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", "vpn-director-telegram-bot")
	if asset {
		req.Header.Set("Accept", assetMediaType)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if asset {
		if mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mediaType == "application/json" {
			return errors.New("GitHub sent the asset's description instead of the file")
		}
	}

	// Stage beside the target and publish with a rename: between the caller's
	// claim check and this write sits the whole HTTP round trip, and a step 2
	// that lost the update directory in that window would otherwise truncate the
	// file of the retry that took it over. telegram-bot.md states the rule as
	// the check being repeated before every file step 2 writes.
	tmp, err := os.CreateTemp(filepath.Dir(target), stagingPattern(target))
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()

	// Limit download size to prevent memory exhaustion. One byte past the limit,
	// so an oversized body is detected by what was written rather than by probing
	// the reader afterwards: a Read that returns (0, nil) is allowed to, and would
	// have let a file truncated at exactly the limit pass as complete.
	limitedReader := io.LimitReader(resp.Body, maxFileSize+1)
	written, err := io.Copy(tmp, limitedReader)
	if err != nil {
		return fmt.Errorf("write file: %w", err)
	}

	if written > maxFileSize {
		return fmt.Errorf("file exceeds maximum size (%d MB)", maxFileSize/1024/1024)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write file: %w", err)
	}

	// os.CreateTemp creates 0600; the published file keeps the 0644 os.Create
	// produced under the daemons' umask, because the update script copies it to
	// a destination that may not exist yet and cp would carry this mode over.
	if err := os.Chmod(tmpName, 0644); err != nil {
		return fmt.Errorf("write file: %w", err)
	}

	if err := s.requireClaim(); err != nil {
		return err
	}

	if err := os.Rename(tmpName, target); err != nil {
		return fmt.Errorf("publish file: %w", err)
	}

	return nil
}

// stagingPattern is the os.CreateTemp pattern downloadFile stages target
// under, in target's own directory.
func stagingPattern(target string) string {
	return "." + filepath.Base(target) + ".part"
}

// reapStaging removes what a downloadFile killed mid-copy left under
// stagingPattern(target): its deferred removal never ran, and the stale-lock
// reaper in IsUpdateInProgress otherwise knows only the published names.
func reapStaging(target string) {
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(target), stagingPattern(target)+"*"))
	for _, m := range matches {
		os.Remove(m)
	}
}

// linkOrCopy puts the file src at dst: a hard link when both sit on one
// filesystem, as the installer and files/ do in /tmp, else a copy.
func linkOrCopy(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}
	err := os.Link(src, dst)
	if errors.Is(err, os.ErrExist) {
		// A destination left by an earlier attempt may be a hard link to src
		// itself - to the binary this process is running as. Copying onto it
		// would open src with O_TRUNC under another name and empty it. Take
		// it away and link afresh.
		if err := os.Remove(dst); err != nil {
			return err
		}
		err = os.Link(src, dst)
	}
	if err == nil {
		return nil
	}
	// Another filesystem, typically: fall back to a copy, and say what the
	// link refused if that fails too.
	if copyErr := copyExecutable(src, dst); copyErr != nil {
		return fmt.Errorf("link: %v; copy: %w", err, copyErr)
	}
	return nil
}

// copyExecutable copies src to dst, which ends up executable whether or not
// it was there before, or does not end up there at all.
func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0755)
	if err != nil {
		return err
	}
	// O_CREATE leaves the mode of a file that is already there alone, and the
	// update script installs this one as a daemon it then starts.
	if err := out.Chmod(0755); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		// Whatever reached the disk is half a binary under the name of a
		// whole one: the script would install it and the daemon would not
		// start.
		os.Remove(dst)
		return err
	}
	return nil
}
