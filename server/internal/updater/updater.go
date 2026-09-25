package updater

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zinin/vpn-director/server/internal/platform"
)

// Update directory paths.
const (
	UpdateDir = "/tmp/vpn-director-update"
	// FilesDirName is where a download lands inside the update directory. It
	// has a name of its own because the directory is injectable in tests and
	// because the bot's startup notifier clears this subdirectory once a
	// failed update has been accounted for.
	FilesDirName = "files"
	FilesDir     = UpdateDir + "/" + FilesDirName
	// LockFileName is how an attempt claims the update directory: created by
	// updateflow.Start before anything is downloaded, republished by the
	// script under its own PID, removed when the attempt is over. The bot's
	// startup notifier claims it too, for as long as it takes to clear the
	// payload of an attempt that failed.
	LockFileName = "lock"
	LockFile     = UpdateDir + "/" + LockFileName
	NotifyFile   = UpdateDir + "/notify.json"
	ScriptFile   = UpdateDir + "/update.sh"
	// InstallerName is where step 1 of a self-update puts the new release's
	// binary it runs (selfupdate.go). Deliberately no daemon's name: pidof and
	// killall in the init scripts match the process name, and a process called
	// telegram-bot would pass for the running daemon.
	InstallerName = "installer"
	InstallerFile = UpdateDir + "/" + InstallerName
)

// ErrLockExists is returned by CreateLock when the lock file is already there.
// Start maps it to updateflow.ErrInProgress so a second caller gets 409, not 500.
var ErrLockExists = errors.New("lock file already exists (update in progress)")

// Release represents a GitHub release.
type Release struct {
	TagName string
	Body    string // Release notes / changelog
	Assets  []Asset
}

// Asset represents a downloadable file in a release.
type Asset struct {
	Name string
	// DownloadURL is the asset's address on the GitHub API; downloadAsset
	// asks it for the file.
	DownloadURL string
}

// Daemon describes an updatable daemon. Name is the release asset prefix
// (<Name>-<arch>), the file name under files/ and the monit service name the
// update script unmonitors and re-monitors; Binary is where the update script
// installs it and what `pgrep -f` matches on; InitScript is the Entware script
// that starts and stops it.
type Daemon struct {
	Name       string
	Binary     string
	InitScript string
}

// Daemon names, as the release assets and the files/ payload spell them.
// Step 1 of a self-update prefers the asset of the daemon it runs in; step 2
// takes that daemon's payload binary from itself (selfupdate.go).
const (
	DaemonBot   = "telegram-bot"
	DaemonWebUI = "webui"
)

// Daemons lists every daemon a release ships. DownloadRelease fetches one
// binary per entry and the update script restarts the entries that were
// running before the update. This table is the single source of truth: the
// downloader, the script template and install.sh must not drift apart.
var Daemons = []Daemon{
	{Name: DaemonBot, Binary: "/opt/vpn-director/telegram-bot", InitScript: "S98telegram-bot"},
	{Name: DaemonWebUI, Binary: "/opt/vpn-director/webui", InitScript: "S98vpn-director-webui"},
}

// Updater defines the interface for update operations.
type Updater interface {
	// GetLatestRelease fetches the latest release info from GitHub.
	GetLatestRelease(ctx context.Context) (*Release, error)

	// ShouldUpdate checks if currentVersion is older than latestTag.
	// Returns an error if either version can't be parsed (dev handled by caller).
	ShouldUpdate(currentVersion, latestTag string) (bool, error)

	// IsUpdateInProgress checks if lock file exists and process is alive.
	IsUpdateInProgress() bool

	// CreateLock creates lock file with current PID.
	CreateLock() error

	// RemoveLock removes the lock file.
	RemoveLock()

	// CleanFiles removes the files/ directory.
	CleanFiles()

	// Handover runs step 1 of a self-update: the new release's binary installs
	// its own release (selfupdate.go). nil means the update script started.
	Handover(ctx context.Context, release *Release, opts RunOptions, progress func(string)) error
}

// Service implements the Updater interface.
type Service struct {
	httpClient         *http.Client
	baseURL            string                 // Injectable for testing, empty = default GitHub API
	rawBaseURL         string                 // Injectable for testing, empty = raw.githubusercontent.com
	lockFile           string                 // Configurable for testing
	updateDir          string                 // Configurable for testing
	scriptFile         string                 // Configurable for testing
	archSuffix         string                 // Injectable for testing, empty = derived from runtime.GOARCH
	shell              string                 // Injectable for testing, empty = /bin/sh
	platform           string                 // Injectable: the manifest tag of this router's platform, detected when empty
	daemon             string                 // The daemon this process is: Handover prefers its asset, step 2 links itself for it
	selfBinary         string                 // Step 2 only: this executable, taken as the payload binary of daemon
	parentPID          func() int             // Injectable for testing, nil = os.Getppid
	executable         func() (string, error) // Injectable for testing, nil = os.Executable
	checkClaim         func() error           // Step 2 only: asked before each file it writes, nil = nothing to check
	handoverTimeout    time.Duration          // Injectable for testing, 0 = defaultHandoverTimeout
	installerWaitDelay time.Duration          // Injectable for testing, 0 = defaultInstallerWaitDelay
}

// Verify Service implements Updater interface.
var _ Updater = (*Service)(nil)

// New creates a new Service with default http.Client.
// No global timeout is set - per-request timeouts are used instead.
func New() *Service {
	return &Service{
		httpClient: &http.Client{},
	}
}

// NewWithBaseURL creates a new Service with a custom base URL for testing.
func NewWithBaseURL(baseURL string) *Service {
	return &Service{
		httpClient: &http.Client{},
		baseURL:    baseURL,
	}
}

// NewForDaemon creates a Service for the daemon named name (DaemonBot or
// DaemonWebUI). The daemons build their update flows with it, because a
// self-update hands over to the new release's binary of the daemon it runs in.
func NewForDaemon(name string) *Service {
	s := New()
	s.daemon = name
	return s
}

// getLockFile returns the lock file path.
func (s *Service) getLockFile() string {
	if s.lockFile != "" {
		return s.lockFile
	}
	return LockFile
}

// getUpdateDir returns the update directory path.
func (s *Service) getUpdateDir() string {
	if s.updateDir != "" {
		return s.updateDir
	}
	return UpdateDir
}

// getFilesDir returns the files directory path.
func (s *Service) getFilesDir() string {
	return filepath.Join(s.getUpdateDir(), FilesDirName)
}

// getScriptFile returns the script file path.
func (s *Service) getScriptFile() string {
	if s.scriptFile != "" {
		return s.scriptFile
	}
	return ScriptFile
}

// getInstallerFile returns where step 1 downloads the new release's binary.
func (s *Service) getInstallerFile() string {
	return filepath.Join(s.getUpdateDir(), InstallerName)
}

// getHandoverTimeout returns how long step 1 waits for step 2.
func (s *Service) getHandoverTimeout() time.Duration {
	if s.handoverTimeout > 0 {
		return s.handoverTimeout
	}
	return defaultHandoverTimeout
}

// getInstallerWaitDelay returns how long step 1 waits for step 2's output
// once step 2 has exited or been killed.
func (s *Service) getInstallerWaitDelay() time.Duration {
	if s.installerWaitDelay > 0 {
		return s.installerWaitDelay
	}
	return defaultInstallerWaitDelay
}

// getShell returns the interpreter that runs the update script. Tests point
// it at a path that does not exist, so the unit suite never launches the real
// script against the machine running go test.
func (s *Service) getShell() string {
	if s.shell != "" {
		return s.shell
	}
	return "/bin/sh"
}

// SetPlatform tells the Service which platform's files to install. The
// daemons call it with what they resolved at startup (--platform or
// detection); step 2 of a self-update takes the platform step 1 passed it as
// --platform, and detects only when that flag is absent.
func (s *Service) SetPlatform(name string) { s.platform = name }

// getPlatform returns the manifest tag of the platform this daemon runs on:
// the one it was told, else the one internal/platform detects. No default:
// a router this cannot name would otherwise get another firmware's hooks.
func (s *Service) getPlatform() (string, error) {
	if s.platform != "" {
		return s.platform, nil
	}
	p, err := platform.Detect()
	if err != nil {
		return "", err
	}
	return p.Name, nil
}

// getParentPID returns the PID of the process that started this one: for
// step 2 of a self-update, the daemon running step 1.
func (s *Service) getParentPID() int {
	if s.parentPID != nil {
		return s.parentPID()
	}
	return os.Getppid()
}

// getExecutable returns the path of the running binary.
func (s *Service) getExecutable() (string, error) {
	if s.executable != nil {
		return s.executable()
	}
	return os.Executable()
}

// IsUpdateInProgress checks if a lock file exists and the process is still alive.
// If the process is dead, the stale lock and what its owner left behind - the
// installer, its .part and files/ - are removed, update.log and notify.json
// stay, and false is returned.
func (s *Service) IsUpdateInProgress() bool {
	lockFile := s.getLockFile()

	pid, err := readLockPID(lockFile)
	if err != nil {
		// A lock file that is there but names no PID is garbage, not a claim:
		// remove it. One that cannot be read at all - usually because there
		// is none - is nobody's to remove.
		var invalid *strconv.NumError
		if errors.As(err, &invalid) {
			os.Remove(lockFile)
		}
		return false
	}

	// Check if process is alive using signal 0
	err = syscall.Kill(pid, 0)
	if err != nil {
		// EPERM means process exists but we don't have permission to signal it
		// This still means the process is alive
		if errors.Is(err, syscall.EPERM) {
			return true
		}
		// Process is dead: reap what its owner left behind, not only the
		// lock. A step 1 that died mid-handover - out of memory in the
		// two-process window is the plausible way - never ran its deferred
		// removals, and startup.cleanup only acts when notify.json exists, so
		// the payload would sit in tmpfs until the next reboot. A dead PID
		// means nobody writes into the directory any more: a re-parented
		// step 2 stops on its own claim check. update.log and notify.json are
		// left alone - they are what a later report is made of. The lock goes
		// last, as in cleanUpOwned, so a Start that sees the stale lock
		// meanwhile also reaps rather than claims a directory still being
		// emptied.
		os.Remove(s.getInstallerFile())
		os.Remove(s.getInstallerFile() + ".part")
		reapStaging(s.getInstallerFile() + ".part")
		s.CleanFiles()
		os.Remove(lockFile)
		return false
	}

	return true
}

// lockNamesPID reports whether the lock file holds exactly pid. Step 1 of a
// self-update asks it about itself before it cleans up after a failure; step 2
// asks it about its parent before it acts.
func (s *Service) lockNamesPID(pid int) bool {
	got, err := readLockPID(s.getLockFile())
	return err == nil && got == pid
}

// errClaimLost is the refusal a writer of the update directory gives when the
// claim it works under is gone. One value, so that a caller between the
// refusal and the user can tell it from a download failure and pass it on
// untouched: the user is told the claim went, not which loop noticed.
var errClaimLost = errors.New("the update lock does not name the process that started this step")

// requireClaim refuses a write into the update directory once the claim the
// writer works under is gone. Only step 2 sets the check: the daemon that
// started it can die mid-download - out of memory on a 256 MB router, now
// that a second Go process runs during it, is the plausible way - and step 2
// is then re-parented and downloads on into a directory a retry may have
// taken over, truncating what that retry has already fetched. Step 1's own
// Service holds the claim rather than working under one, and leaves this nil.
func (s *Service) requireClaim() error {
	if s.checkClaim == nil {
		return nil
	}
	return s.checkClaim()
}

// readLockPID returns the PID a lock file names. The trailing newline is what
// the update script's printf leaves when it republishes its own PID. Reading
// the claim is one function because two readers disagreeing on what a lock
// says would judge the same directory differently.
func readLockPID(lockFile string) (int, error) {
	data, err := os.ReadFile(lockFile)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

// CreateLockAt claims the update directory by publishing lockFile with the
// PID of the calling process. Every writer of that directory goes through it:
// Start before it downloads, and the bot's startup notifier before it clears
// the payload of an attempt that failed. ErrLockExists means someone else
// holds the claim and the caller owns nothing in there.
//
// The claim is made, never looked for. Whoever only checks whether the lock is
// there leaves the window between that check and its own work for a second
// claimant to start in; linking a written file into place has exactly one of
// two callers arriving together win.
func CreateLockAt(lockFile string) error {
	// Ensure directory exists
	dir := filepath.Dir(lockFile)
	// 0755 root-owned: the update directory holds a script this process then
	// runs as root. On Asuswrt-Merlin there is no unprivileged local user to
	// defend against, which is why this is a comment and not a mechanism.
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create lock directory: %w", err)
	}

	// Publish the lock with its PID already in it. O_CREATE|O_EXCL alone
	// leaves the file empty between the open and the write, and a status check
	// landing there parses no PID, judges the lock garbage and deletes it -
	// CreateLock would then report success on a lock that no longer exists,
	// and the next Start would begin a second update that wipes the shared
	// files/ directory under this one. Link fails when the target exists, so
	// the exclusivity O_EXCL gave us is kept.
	tmp, err := os.CreateTemp(dir, "lock.*")
	if err != nil {
		return fmt.Errorf("failed to create lock file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the link is in place and this returns

	if _, err := tmp.WriteString(strconv.Itoa(os.Getpid())); err != nil {
		tmp.Close()
		return fmt.Errorf("failed to write PID to lock file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to write PID to lock file: %w", err)
	}
	// CreateTemp gives 0600; the lock has always been world-readable.
	if err := os.Chmod(tmpName, 0644); err != nil {
		return fmt.Errorf("failed to create lock file: %w", err)
	}

	if err := os.Link(tmpName, lockFile); err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrLockExists
		}
		return fmt.Errorf("failed to create lock file: %w", err)
	}

	return nil
}

// CreateLock claims the update directory for this Service.
func (s *Service) CreateLock() error { return CreateLockAt(s.getLockFile()) }

// RemoveLockAt drops the claim at lockFile. Errors are ignored: the claim of a
// process that is gone is reaped by the next IsUpdateInProgress, which finds
// nothing alive behind the PID in it.
func RemoveLockAt(lockFile string) { os.Remove(lockFile) }

// RemoveLock removes the lock file. Errors are ignored.
func (s *Service) RemoveLock() { RemoveLockAt(s.getLockFile()) }

// CleanFiles removes the files/ directory. Errors are ignored.
func (s *Service) CleanFiles() {
	os.RemoveAll(s.getFilesDir())
}

// RunUpdateScript is implemented in script.go
