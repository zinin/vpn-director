package updater

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestIsUpdateInProgress_NoLockFile(t *testing.T) {
	tempDir := t.TempDir()
	s := &Service{lockFile: filepath.Join(tempDir, "lock")}

	if s.IsUpdateInProgress() {
		t.Error("IsUpdateInProgress() = true, want false (no lock file)")
	}
}

// writeUpdateLeftovers fills the update directory the way an attempt in flight
// leaves it: the installer step 1 downloads, a half-written one, the payload,
// and the two files a later report is made of.
func writeUpdateLeftovers(t *testing.T, s *Service) (gone, kept []string) {
	t.Helper()
	if err := os.MkdirAll(s.getFilesDir(), 0755); err != nil {
		t.Fatalf("mkdir files dir: %v", err)
	}
	// The last entry is what downloadFile stages the installer under for the
	// whole HTTP round trip (os.CreateTemp beside installer.part); an owner
	// killed mid-download never ran the deferred removal.
	gone = []string{
		s.getInstallerFile(),
		s.getInstallerFile() + ".part",
		filepath.Join(s.getFilesDir(), "marker"),
		filepath.Join(s.getUpdateDir(), ".installer.part.part4215863503"),
	}
	kept = []string{filepath.Join(s.getUpdateDir(), "update.log"), filepath.Join(s.getUpdateDir(), "notify.json")}
	for _, path := range append(append([]string(nil), gone...), kept...) {
		if err := os.WriteFile(path, []byte("x"), 0644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return gone, kept
}

func TestIsUpdateInProgress_ValidLock(t *testing.T) {
	tempDir := t.TempDir()
	lockFile := filepath.Join(tempDir, "lock")
	s := &Service{lockFile: lockFile, updateDir: tempDir}
	gone, kept := writeUpdateLeftovers(t, s)

	// Create lock with current PID (which is alive)
	pid := os.Getpid()
	if err := os.WriteFile(lockFile, []byte(strconv.Itoa(pid)), 0644); err != nil {
		t.Fatalf("Failed to create lock file: %v", err)
	}

	if !s.IsUpdateInProgress() {
		t.Error("IsUpdateInProgress() = false, want true (valid lock with alive process)")
	}

	// The owner is alive and still downloading: nothing of its is anyone
	// else's to remove.
	for _, path := range append(append([]string(nil), gone...), kept...) {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s was removed from under a live update: %v", filepath.Base(path), err)
		}
	}
}

func TestIsUpdateInProgress_StaleLock_InvalidPID(t *testing.T) {
	tempDir := t.TempDir()
	lockFile := filepath.Join(tempDir, "lock")
	s := &Service{lockFile: lockFile}

	// Create lock with invalid PID content
	if err := os.WriteFile(lockFile, []byte("not-a-number"), 0644); err != nil {
		t.Fatalf("Failed to create lock file: %v", err)
	}

	if s.IsUpdateInProgress() {
		t.Error("IsUpdateInProgress() = true, want false (invalid PID)")
	}

	// Lock file should be removed
	if _, err := os.Stat(lockFile); !os.IsNotExist(err) {
		t.Error("Stale lock file was not removed")
	}
}

func TestIsUpdateInProgress_StaleLock_DeadProcess(t *testing.T) {
	tempDir := t.TempDir()
	lockFile := filepath.Join(tempDir, "lock")
	s := &Service{lockFile: lockFile, updateDir: tempDir}
	gone, kept := writeUpdateLeftovers(t, s)

	// Create lock with a PID that almost certainly doesn't exist
	// Use a very high PID that's unlikely to be in use
	if err := os.WriteFile(lockFile, []byte("999999999"), 0644); err != nil {
		t.Fatalf("Failed to create lock file: %v", err)
	}

	if s.IsUpdateInProgress() {
		t.Error("IsUpdateInProgress() = true, want false (dead process)")
	}

	// Lock file should be removed
	if _, err := os.Stat(lockFile); !os.IsNotExist(err) {
		t.Error("Stale lock file was not removed")
	}

	// The owner is dead and never ran its own cleanup, so its payload is
	// reaped here; update.log and notify.json are what a later report reads.
	for _, path := range gone {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s was left behind by a dead owner's cleanup", filepath.Base(path))
		}
	}
	for _, path := range kept {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s must survive the stale-lock cleanup: %v", filepath.Base(path), err)
		}
	}
}

func TestCreateLock_Success(t *testing.T) {
	tempDir := t.TempDir()
	lockFile := filepath.Join(tempDir, "subdir", "lock")
	s := &Service{lockFile: lockFile}

	if err := s.CreateLock(); err != nil {
		t.Fatalf("CreateLock() error = %v", err)
	}

	// Verify lock file exists with correct PID
	data, err := os.ReadFile(lockFile)
	if err != nil {
		t.Fatalf("Failed to read lock file: %v", err)
	}

	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatalf("Lock file contains invalid PID: %v", err)
	}

	if pid != os.Getpid() {
		t.Errorf("Lock file PID = %d, want %d", pid, os.Getpid())
	}
}

func TestCreateLock_AlreadyExists(t *testing.T) {
	tempDir := t.TempDir()
	lockFile := filepath.Join(tempDir, "lock")
	s := &Service{lockFile: lockFile}

	// Create lock file first
	if err := os.WriteFile(lockFile, []byte("12345"), 0644); err != nil {
		t.Fatalf("Failed to create existing lock file: %v", err)
	}

	// Try to create lock - should fail
	err := s.CreateLock()
	if !errors.Is(err, ErrLockExists) {
		t.Errorf("CreateLock() error = %v, want ErrLockExists", err)
	}

	// Original content should be preserved
	data, _ := os.ReadFile(lockFile)
	if string(data) != "12345" {
		t.Errorf("Original lock content was modified: got %q, want %q", string(data), "12345")
	}
}

func TestCreateLock_AtomicRaceCondition(t *testing.T) {
	tempDir := t.TempDir()
	lockFile := filepath.Join(tempDir, "lock")

	// Simulate race: two services try to create lock
	s1 := &Service{lockFile: lockFile}
	s2 := &Service{lockFile: lockFile}

	// First one succeeds
	if err := s1.CreateLock(); err != nil {
		t.Fatalf("First CreateLock() error = %v", err)
	}

	// Second one must fail
	err := s2.CreateLock()
	if err == nil {
		t.Error("Second CreateLock() should fail (race condition protection)")
	}
}

// The Web UI polls /api/update/status every three seconds while an update
// runs, and Start checks it too. A poll landing inside CreateLock used to find
// the lock created but not yet written, parse no PID, judge it garbage and
// delete it: CreateLock then reported success on a lock that was already gone,
// and the next Start began a second update that wipes the shared files/
// directory under the first.
func TestCreateLock_SurvivesAConcurrentProgressCheck(t *testing.T) {
	dir := t.TempDir()
	s := &Service{lockFile: filepath.Join(dir, "lock")}

	for i := 0; i < 500; i++ {
		done := make(chan struct{})
		go func() {
			defer close(done)
			for j := 0; j < 200; j++ {
				s.IsUpdateInProgress()
			}
		}()

		if err := s.CreateLock(); err != nil {
			t.Fatalf("CreateLock() error = %v", err)
		}
		<-done

		if _, err := os.Stat(s.getLockFile()); err != nil {
			t.Fatalf("round %d: the lock did not survive a status check beside it: %v", i, err)
		}
		s.RemoveLock()
	}
}

// The lock is published by linking a written temp file into place, so nothing
// of that mechanism may be left behind.
func TestCreateLock_LeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	s := &Service{lockFile: filepath.Join(dir, "lock")}

	if err := s.CreateLock(); err != nil {
		t.Fatalf("CreateLock() error = %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read lock directory: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "lock" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("lock directory holds %v, want just the lock", names)
	}
}

func TestRemoveLock(t *testing.T) {
	tempDir := t.TempDir()
	lockFile := filepath.Join(tempDir, "lock")
	s := &Service{lockFile: lockFile}

	// Create lock file
	if err := os.WriteFile(lockFile, []byte("12345"), 0644); err != nil {
		t.Fatalf("Failed to create lock file: %v", err)
	}

	// Remove it
	s.RemoveLock()

	// Verify it's gone
	if _, err := os.Stat(lockFile); !os.IsNotExist(err) {
		t.Error("RemoveLock() did not remove the lock file")
	}
}

func TestRemoveLock_NoFile(t *testing.T) {
	tempDir := t.TempDir()
	s := &Service{lockFile: filepath.Join(tempDir, "nonexistent")}

	// Should not panic when file doesn't exist
	s.RemoveLock()
}

func TestCleanFiles(t *testing.T) {
	tempDir := t.TempDir()
	s := &Service{updateDir: tempDir}

	// Create files directory with some content
	filesDir := filepath.Join(tempDir, "files")
	if err := os.MkdirAll(filepath.Join(filesDir, "subdir"), 0755); err != nil {
		t.Fatalf("Failed to create files directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(filesDir, "test.txt"), []byte("test"), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// Clean it
	s.CleanFiles()

	// Verify it's gone
	if _, err := os.Stat(filesDir); !os.IsNotExist(err) {
		t.Error("CleanFiles() did not remove the files directory")
	}
}

func TestCleanFiles_NoDirectory(t *testing.T) {
	tempDir := t.TempDir()
	s := &Service{updateDir: filepath.Join(tempDir, "nonexistent")}

	// Should not panic when directory doesn't exist
	s.CleanFiles()
}

func TestGetters_Defaults(t *testing.T) {
	s := &Service{}

	if got := s.getLockFile(); got != LockFile {
		t.Errorf("getLockFile() = %q, want %q", got, LockFile)
	}
	if got := s.getUpdateDir(); got != UpdateDir {
		t.Errorf("getUpdateDir() = %q, want %q", got, UpdateDir)
	}
	if got := s.getFilesDir(); got != FilesDir {
		t.Errorf("getFilesDir() = %q, want %q", got, FilesDir)
	}
	if got := s.getScriptFile(); got != ScriptFile {
		t.Errorf("getScriptFile() = %q, want %q", got, ScriptFile)
	}
	if got := s.getInstallerFile(); got != InstallerFile {
		t.Errorf("getInstallerFile() = %q, want %q", got, InstallerFile)
	}
	if got := s.getHandoverTimeout(); got != defaultHandoverTimeout {
		t.Errorf("getHandoverTimeout() = %v, want %v", got, defaultHandoverTimeout)
	}
}

func TestGetters_Custom(t *testing.T) {
	s := &Service{
		lockFile:   "/custom/lock",
		updateDir:  "/custom/update",
		scriptFile: "/custom/script.sh",
	}

	if got := s.getLockFile(); got != "/custom/lock" {
		t.Errorf("getLockFile() = %q, want %q", got, "/custom/lock")
	}
	if got := s.getUpdateDir(); got != "/custom/update" {
		t.Errorf("getUpdateDir() = %q, want %q", got, "/custom/update")
	}
	if got := s.getFilesDir(); got != "/custom/update/files" {
		t.Errorf("getFilesDir() = %q, want %q", got, "/custom/update/files")
	}
	if got := s.getScriptFile(); got != "/custom/script.sh" {
		t.Errorf("getScriptFile() = %q, want %q", got, "/custom/script.sh")
	}
	if got := s.getInstallerFile(); got != "/custom/update/installer" {
		t.Errorf("getInstallerFile() = %q, want %q", got, "/custom/update/installer")
	}
}

// TestDaemons_CarryNoShellMetacharacters pins the assumption update_script.sh
// makes about this table: the entries are pasted into one space-separated
// DAEMONS string and iterated with word splitting, so a space would split an
// entry in two and a glob character would expand against the router's
// filesystem. The script cannot defend itself with `set -f`, because steps 4
// and 5 copy and chmod through globs of their own, so the invariant is pinned
// here, where the table is written.
func TestDaemons_CarryNoShellMetacharacters(t *testing.T) {
	const forbidden = " \t\n|*?[]$`\"'\\"
	for _, d := range Daemons {
		fields := []struct{ name, value string }{
			{"Name", d.Name},
			{"Binary", d.Binary},
			{"InitScript", d.InitScript},
		}
		for _, f := range fields {
			if strings.ContainsAny(f.value, forbidden) {
				t.Errorf("Daemon %q field %s = %q contains a character the update script's word splitting cannot survive",
					d.Name, f.name, f.value)
			}
			if f.value == "" {
				t.Errorf("Daemon %q field %s is empty", d.Name, f.name)
			}
		}
	}
}

// The bot's startup notifier clears the payload a failed update left behind,
// and it is handed paths rather than a Service. Its claim still has to be the
// one Start makes, down to the PID a status check reads back out of it -
// otherwise the two do not exclude each other at all.
func TestCreateLockAt_ClaimsTheDirectoryForTheCallingProcess(t *testing.T) {
	lockFile := filepath.Join(t.TempDir(), "lock")

	if err := CreateLockAt(lockFile); err != nil {
		t.Fatalf("CreateLockAt() error = %v", err)
	}
	data, err := os.ReadFile(lockFile)
	if err != nil {
		t.Fatalf("read the claim: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid != os.Getpid() {
		t.Errorf("the claim holds %q, want the PID %d", data, os.Getpid())
	}

	if err := CreateLockAt(lockFile); !errors.Is(err, ErrLockExists) {
		t.Errorf("second CreateLockAt() error = %v, want ErrLockExists", err)
	}
	again, err := os.ReadFile(lockFile)
	if err != nil || string(again) != string(data) {
		t.Errorf("a refused claim disturbed the lock: %q (%v), was %q", again, err, data)
	}

	RemoveLockAt(lockFile)
	if _, err := os.Stat(lockFile); !os.IsNotExist(err) {
		t.Fatalf("RemoveLockAt() left the claim behind: %v", err)
	}
	if err := CreateLockAt(lockFile); err != nil {
		t.Errorf("CreateLockAt() after a release error = %v, want the directory free again", err)
	}
}

// Step 1 of a self-update prefers the release asset of the daemon it runs in,
// and step 2 takes that daemon's payload binary from itself. Both key on these
// names, so they must be the names the daemon table ships under.
func TestDaemonConstants_NameEntriesOfTheTable(t *testing.T) {
	for _, name := range []string{DaemonBot, DaemonWatchd, DaemonWebUI} {
		found := false
		for _, d := range Daemons {
			if d.Name == name {
				found = true
			}
		}
		if !found {
			t.Errorf("no Daemons entry named %q", name)
		}
	}
}

func TestDaemons_OrderAndWatchdPaths(t *testing.T) {
	want := []string{DaemonBot, DaemonWatchd, DaemonWebUI}
	if len(Daemons) != len(want) {
		t.Fatalf("Daemons has %d entries, want %d", len(Daemons), len(want))
	}
	for i, name := range want {
		if Daemons[i].Name != name {
			t.Errorf("Daemons[%d].Name = %q, want %q", i, Daemons[i].Name, name)
		}
	}
	if got := Daemons[1]; got.Binary != "/opt/vpn-director/vpn-director-watchd" || got.InitScript != "S98vpn-director-watchd" {
		t.Errorf("watchd paths = %+v", got)
	}
}

func TestNewForDaemon_RemembersTheDaemon(t *testing.T) {
	if got := NewForDaemon(DaemonWebUI).daemon; got != DaemonWebUI {
		t.Errorf("NewForDaemon(webui).daemon = %q", got)
	}
	if got := New().daemon; got != "" {
		t.Errorf("New().daemon = %q, want none", got)
	}
}

func TestLockNamesPID(t *testing.T) {
	lockFile := filepath.Join(t.TempDir(), "lock")
	s := &Service{lockFile: lockFile}

	if s.lockNamesPID(4242) {
		t.Error("lockNamesPID() = true without a lock file")
	}
	for content, want := range map[string]bool{
		"4242":   true,
		"4242\n": true, // the update script republishes its PID with printf '%s\n'
		"4243":   false,
		"":       false,
		"junk":   false,
	} {
		if err := os.WriteFile(lockFile, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		if got := s.lockNamesPID(4242); got != want {
			t.Errorf("lock %q: lockNamesPID(4242) = %v, want %v", content, got, want)
		}
	}
}
