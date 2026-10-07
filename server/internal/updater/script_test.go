package updater

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// validOpts returns options that pass every validation, so a test can flip a
// single field and assert that this one field is what got rejected.
func validOpts() RunOptions {
	return RunOptions{OldVersion: "v1.0.0", NewVersion: "v1.1.0", ChatID: 123, Initiator: "bot"}
}

// noExecService returns a Service whose interpreter path does not exist, so
// RunUpdateScript writes the script and then fails at exec. Without the seam
// these tests launch the real update script - pgrep and pkill -9 over the
// whole process table, cp over /opt - on the machine running go test.
func noExecService(t *testing.T) *Service {
	t.Helper()

	dir := t.TempDir()
	return &Service{
		updateDir:  dir,
		scriptFile: filepath.Join(dir, "update.sh"),
		shell:      filepath.Join(dir, "no-such-interpreter"),
	}
}

// assertExecRefused checks that RunUpdateScript got as far as launching the
// script and no further. A nil error would mean a real shell took the
// generated script and ran it.
func assertExecRefused(t *testing.T, s *Service, err error) {
	t.Helper()

	if err == nil {
		t.Fatal("RunUpdateScript() succeeded, so a real shell just ran the update script")
	}
	if !strings.Contains(err.Error(), s.shell) {
		t.Fatalf("RunUpdateScript() error = %v, want the exec of %s to fail", err, s.shell)
	}
}

// testManifest is the payload manifest the script tests install from. It has
// the shape of the real router/files.manifest, the daemon init scripts
// included: those land in both the FILES table, which installs them, and the
// DAEMONS table, which starts and stops them.
const testManifest = "common router/opt/vpn-director/vpn-director.sh\n" +
	"common router/opt/vpn-director/lib/common.sh\n" +
	"common router/opt/vpn-director/vpn-director.json.template\n" +
	"common router/opt/etc/init.d/S99vpn-director\n" +
	"common router/opt/etc/init.d/S98telegram-bot\n" +
	"common router/opt/etc/init.d/S98vpn-director-watchd\n" +
	"common router/opt/etc/init.d/S98vpn-director-webui\n" +
	"merlin router/jffs/scripts/firewall-start\n" +
	"keenetic router/opt/etc/ndm/netfilter.d/50-vpn-director.sh\n"

// writeTestManifest gives the update payload the manifest generateScript
// reads; DownloadRelease writes it there in production. The golden test
// renders in its owned directory and normalizes that prefix to the default
// paths, so its deterministic output needs no shared update directory.
func writeTestManifest(t *testing.T, s *Service) {
	t.Helper()
	dir := s.getFilesDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir files dir: %v", err)
	}
	path := filepath.Join(dir, "files.manifest")
	if err := os.WriteFile(path, []byte(testManifest), 0644); err != nil {
		t.Fatalf("write files.manifest: %v", err)
	}
	t.Cleanup(func() {
		os.Remove(path)
		// Both only succeed while they are empty, which is the only case
		// where this test created them.
		os.Remove(dir)
		os.Remove(s.getUpdateDir())
	})
}

func TestGenerateScript_CopiesManifestFiles(t *testing.T) {
	s := &Service{updateDir: t.TempDir()}
	writeTestManifest(t, s)

	script, err := s.generateScript(validOpts())
	if err != nil {
		t.Fatalf("generateScript() error = %v", err)
	}
	for _, want := range []string{
		"opt/vpn-director/lib/common.sh|/opt/vpn-director/lib/common.sh|x",
		"opt/vpn-director/vpn-director.json.template|/opt/vpn-director/vpn-director.json.template|-",
		"opt/etc/init.d/S99vpn-director|/opt/etc/init.d/S99vpn-director|x",
		"jffs/scripts/firewall-start|/jffs/scripts/firewall-start|x",
		`mkdir -p "$(dirname "$dst")"`,
		`cp -f "$FILES_DIR/$src" "$dst"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q", want)
		}
	}
	for _, stale := range []string{
		"netfilter.d",
		`cp -f "$FILES_DIR/jffs/scripts/"*`,
		`cp -f "$FILES_DIR/opt/vpn-director/lib/"*.sh`,
		"chmod +x /jffs/scripts/firewall-start",
	} {
		if strings.Contains(script, stale) {
			t.Errorf("script still contains %q", stale)
		}
	}
}

// A table the shell cannot split back into files installs nothing, and a
// missing chmod ships a shell script the init system cannot run. Both are
// invisible to a containment check, so the generated loop is run here against
// a sandbox: every source the table names is copied to its destination, with
// the executable bit on exactly the entries marked "x".
func TestGenerateScript_FileTableInstallsWhatItNames(t *testing.T) {
	s := &Service{updateDir: t.TempDir()}
	writeTestManifest(t, s)

	script, err := s.generateScript(validOpts())
	if err != nil {
		t.Fatalf("generateScript() error = %v", err)
	}
	table := regexp.MustCompile(`(?m)^FILES="([^"]*)"$`).FindStringSubmatch(script)
	if table == nil {
		t.Fatal("no FILES table in the generated script")
	}
	loop := regexp.MustCompile(`(?ms)^for entry in \$FILES; do.*?^done$`).FindString(script)
	if loop == "" {
		t.Fatal("no copy loop over $FILES in the generated script")
	}

	// Give the payload every source the table names.
	for _, entry := range strings.Fields(table[1]) {
		src := filepath.Join(s.getFilesDir(), strings.SplitN(entry, "|", 2)[0])
		if err := os.MkdirAll(filepath.Dir(src), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(src, []byte("payload\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// The destinations are absolute paths on the router, so they are rehomed
	// under a temporary root. Only the table is rewritten - "|/" starts a
	// destination and nothing else - so the loop itself runs verbatim.
	root := t.TempDir()
	snippet := "set -e\nFILES_DIR=" + s.getFilesDir() + "\nFILES=\"" +
		strings.ReplaceAll(table[1], "|/", "|"+root+"/") + "\"\n" + loop + "\n"
	if out, err := exec.Command("/bin/sh", "-c", snippet).CombinedOutput(); err != nil {
		t.Fatalf("the copy loop failed: %v\n%s", err, out)
	}

	for rel, wantExec := range map[string]bool{
		"opt/vpn-director/vpn-director.sh":            true,
		"opt/vpn-director/lib/common.sh":              true,
		"opt/etc/init.d/S99vpn-director":              true,
		"opt/etc/init.d/S98telegram-bot":              true,
		"opt/etc/init.d/S98vpn-director-watchd":       true,
		"jffs/scripts/firewall-start":                 true,
		"opt/vpn-director/vpn-director.json.template": false,
	} {
		fi, err := os.Stat(filepath.Join(root, rel))
		if err != nil {
			t.Errorf("%s was not installed: %v", rel, err)
			continue
		}
		if gotExec := fi.Mode()&0111 != 0; gotExec != wantExec {
			t.Errorf("%s installed as %v, want executable = %v", rel, fi.Mode(), wantExec)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "opt/etc/ndm/netfilter.d/50-vpn-director.sh")); err == nil {
		t.Error("a keenetic-tagged file was installed on a merlin router")
	}
}

func TestGenerateScript_FailsWithoutPayloadManifest(t *testing.T) {
	s := &Service{updateDir: t.TempDir()}
	if _, err := s.generateScript(validOpts()); err == nil || !strings.Contains(err.Error(), "files.manifest") {
		t.Fatalf("generateScript() error = %v, want one naming files.manifest", err)
	}
}

func TestService_ShellDefaultsToBinSh(t *testing.T) {
	// The seam must not change what ships: a router has /bin/sh and nothing
	// else is guaranteed.
	if got := (&Service{}).getShell(); got != "/bin/sh" {
		t.Errorf("getShell() = %q, want /bin/sh", got)
	}
	if got := (&Service{shell: "/bin/busybox"}).getShell(); got != "/bin/busybox" {
		t.Errorf("getShell() = %q, want the injected interpreter", got)
	}
}

func TestGenerateScript(t *testing.T) {
	tmpDir := t.TempDir()

	s := &Service{
		updateDir: tmpDir,
	}
	writeTestManifest(t, s)

	script, err := s.generateScript(RunOptions{ChatID: 123456789, OldVersion: "v1.0.0", NewVersion: "v1.1.0", Initiator: "bot"})
	if err != nil {
		t.Fatalf("generateScript() error = %v", err)
	}

	// Check that variables are embedded correctly
	checks := []string{
		"CHAT_ID=123456789",
		`OLD_VERSION="v1.0.0"`,
		`NEW_VERSION="v1.1.0"`,
		"set -e",
		`pgrep -f "$bin"`, // full binary path, from the daemon table
		"telegram-bot|/opt/vpn-director/telegram-bot|S98telegram-bot",
		"webui|/opt/vpn-director/webui|S98vpn-director-webui",
	}

	for _, check := range checks {
		if !strings.Contains(script, check) {
			t.Errorf("Script missing %q", check)
		}
	}

	// monit commands stay optional
	if !strings.Contains(script, `monit unmonitor "${entry%%|*}" 2>/dev/null || true`) {
		t.Error("Script missing || true for monit unmonitor")
	}
	if !strings.Contains(script, `monit monitor "$name" 2>/dev/null || true`) {
		t.Error("Script missing || true for monit monitor")
	}
	if !strings.Contains(script, "remonitor_running") {
		t.Error("script must remonitor only daemons that were running")
	}
	if !strings.Contains(script, "flock -n 9") {
		t.Error("script must wait for vpn-director.sh before rewriting it")
	}

	own := strings.Index(script, `mv -f "$LOCK_FILE.new" "$LOCK_FILE"`)
	stop := strings.Index(script, "# 3. Stop the running daemons")
	if own < 0 {
		t.Error("script must take lock ownership before it stops the initiator")
	} else if stop < 0 || own > stop {
		t.Error("lock ownership must be taken before the stop loop")
	}
	if !strings.Contains(script, `rm -rf "$FILES_DIR"`) {
		t.Error("script must drop files/ after the copy so a Web UI-only router does not keep binaries in tmpfs")
	}
	okAt := strings.Index(script, "write_notify ok")
	exceptAt := strings.Index(script, `start_except "$NOTIFY_INIT"`)
	if okAt < 0 || exceptAt < 0 || exceptAt > okAt {
		t.Error("write_notify ok must come after the non-bot daemons have started, or the bot can announce success while Web UI is down")
	}

	// Check that cp commands do NOT have || true (critical commands)
	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "cp -f") {
			if strings.HasSuffix(trimmed, "|| true") {
				t.Errorf("cp command should not have || true: %s", line)
			}
		}
	}
}

func TestGenerateScript_PathsCorrect(t *testing.T) {
	tmpDir := t.TempDir()

	s := &Service{
		updateDir: tmpDir,
	}
	writeTestManifest(t, s)

	script, err := s.generateScript(RunOptions{ChatID: 999, OldVersion: "v1.0.0", NewVersion: "v2.0.0", Initiator: "bot"})
	if err != nil {
		t.Fatalf("generateScript() error = %v", err)
	}

	// Verify paths use the custom updateDir
	expectedPaths := []string{
		`UPDATE_DIR="` + tmpDir + `"`,
		`FILES_DIR="` + tmpDir + `/files"`,
		`NOTIFY_FILE="` + tmpDir + `/notify.json"`,
		`LOCK_FILE="` + tmpDir + `/lock"`,
	}

	for _, expected := range expectedPaths {
		if !strings.Contains(script, expected) {
			t.Errorf("Script missing path %q", expected)
		}
	}
}

func TestGenerateScript_EmbedsInitiator(t *testing.T) {
	tmpDir := t.TempDir()
	s := &Service{updateDir: tmpDir}
	writeTestManifest(t, s)

	script, err := s.generateScript(RunOptions{
		OldVersion: "v1.0.0", NewVersion: "v1.1.0", ChatID: 0, Initiator: "webui",
	})
	if err != nil {
		t.Fatalf("generateScript() error = %v", err)
	}
	if !strings.Contains(script, `INITIATOR="webui"`) {
		t.Error("script must carry the initiator so notify.json can name it")
	}
	if !strings.Contains(script, "CHAT_ID=0") {
		t.Error("a Web UI update has chat_id 0")
	}
}

func TestGenerateScript_CoversEveryDaemon(t *testing.T) {
	s := &Service{updateDir: t.TempDir()}
	writeTestManifest(t, s)
	script, err := s.generateScript(validOpts())
	if err != nil {
		t.Fatalf("generateScript() error = %v", err)
	}

	for _, d := range Daemons {
		for _, want := range []string{d.Name, d.Binary, d.InitScript} {
			if !strings.Contains(script, want) {
				t.Errorf("script missing %q for daemon %s", want, d.Name)
			}
		}
	}

	// The tables drive every loop, and both reach an init script only through
	// parameter expansion - ${entry##*|} in the daemon loops, $dst in the copy
	// loop. So an init script spelled out in a line the shell executes is a
	// hardcoded call, and a hardcoded call silently skips the other daemon.
	// The two table lines are where the names belong; comments, as elsewhere
	// in this file, are free to name what they explain.
	for i, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") ||
			strings.HasPrefix(trimmed, `DAEMONS="`) || strings.HasPrefix(trimmed, `FILES="`) {
			continue
		}
		for _, d := range Daemons {
			if strings.Contains(line, d.InitScript) {
				t.Errorf("line %d spells out the init script %s instead of taking it from a table: %s", i+1, d.InitScript, line)
			}
		}
	}
}

func TestGenerateScript_RecoveryOnFailure(t *testing.T) {
	s := &Service{updateDir: t.TempDir()}
	writeTestManifest(t, s)
	script, err := s.generateScript(validOpts())
	if err != nil {
		t.Fatalf("generateScript() error = %v", err)
	}

	if !strings.Contains(script, "trap on_exit EXIT") {
		t.Error("script must install the EXIT trap: ash has no ERR trap")
	}

	// Every other needle has to sit inside the handler. Searching the whole
	// script would pass on the happy-path copy of the same line, so removing
	// the step from the recovery path would go unnoticed.
	body := onExitBody(t, script)
	checks := map[string]string{
		"code=$?":             "the EXIT trap must inspect the exit code",
		"set +e":              "recovery must survive its own failing steps",
		"write_notify failed": "a failed update must leave status failed in notify.json",
		"start_running":       "recovery must restart the daemons that were running",
		"remonitor_running":   "recovery must remonitor only daemons that were running",
		`rm -f "$LOCK_FILE"`:  "a failed update must release the lock",
	}
	for needle, why := range checks {
		if !strings.Contains(body, needle) {
			t.Errorf("on_exit missing %q: %s", needle, why)
		}
	}
}

// The lock taken in step 3b lives on the open file description, and a daemon
// started while fd 9 is still open inherits it: /var/lock/vpn-director.lock
// would stay held for as long as that daemon runs, turning every later
// vpn-director.sh into a silent skip (no --wait) or a lock timeout (--wait).
// Both the happy path and the recovery path must release before they start
// anything.
func TestGenerateScript_ReleasesApplyLockBeforeStartingDaemons(t *testing.T) {
	s := &Service{updateDir: t.TempDir()}
	writeTestManifest(t, s)
	script, err := s.generateScript(validOpts())
	if err != nil {
		t.Fatalf("generateScript() error = %v", err)
	}

	if body := functionBody(t, script, "release_apply_lock"); !strings.Contains(body, "exec 9>&-") {
		t.Error("release_apply_lock must close the descriptor, not just drop the lock")
	}

	paths := []struct {
		name  string
		body  string
		start string
	}{
		{"happy path", afterOnExit(t, script), "start_except"},
		{"recovery", onExitBody(t, script), "start_running"},
	}
	for _, p := range paths {
		release := strings.Index(p.body, "release_apply_lock")
		start := strings.Index(p.body, p.start)
		switch {
		case release < 0:
			t.Errorf("%s never releases the apply lock", p.name)
		case start < 0:
			t.Errorf("%s never starts a daemon through %s", p.name, p.start)
		case release > start:
			t.Errorf("%s releases the apply lock after %s, so the started daemon inherits fd 9", p.name, p.start)
		}
	}
}

// POSIX allows exactly one digit in a redirection; a multi-digit descriptor is
// a bash/ksh extension. getShell() runs the generated script with /bin/sh, and
// dash rejects "exec 201>" outright.
func TestGenerateScript_UsesPOSIXFileDescriptors(t *testing.T) {
	s := &Service{updateDir: t.TempDir()}
	writeTestManifest(t, s)
	script, err := s.generateScript(validOpts())
	if err != nil {
		t.Fatalf("generateScript() error = %v", err)
	}

	multiDigit := regexp.MustCompile(`exec\s+[0-9]{2,}[<>]|flock\s+(?:-[a-z]+\s+)*[0-9]{2,}\b`)
	if m := multiDigit.FindString(script); m != "" {
		t.Errorf("script uses the multi-digit file descriptor %q; /bin/sh may be dash, which rejects it", m)
	}
}

// The bot reads notify.json once, on startup. Restarted before the file
// exists it finds nothing, and a running process never looks again, so the
// failure would stay unreported until the next restart.
func TestGenerateScript_CommitsTheFailedStatusBeforeRestarting(t *testing.T) {
	s := &Service{updateDir: t.TempDir()}
	writeTestManifest(t, s)
	script, err := s.generateScript(validOpts())
	if err != nil {
		t.Fatalf("generateScript() error = %v", err)
	}

	body := onExitBody(t, script)
	notify := strings.Index(body, "write_notify failed")
	start := strings.Index(body, "start_running")
	switch {
	case notify < 0:
		t.Fatal("recovery never writes the failed status")
	case start < 0:
		t.Fatal("recovery never restarts the daemons")
	case notify > start:
		t.Error("recovery restarts the daemons before it writes notify.json")
	}
}

// monit starts a stopped daemon on its own check interval. Handing the
// daemons back before the status is committed lets it put the bot up without
// a notify.json to read, and CheckAndSendNotify runs at startup only.
func TestGenerateScript_RestoresMonitLast(t *testing.T) {
	s := &Service{updateDir: t.TempDir()}
	writeTestManifest(t, s)
	script, err := s.generateScript(validOpts())
	if err != nil {
		t.Fatalf("generateScript() error = %v", err)
	}

	paths := []struct {
		name   string
		body   string
		starts string
	}{
		{"happy path", afterOnExit(t, script), "start_except"},
		{"recovery", onExitBody(t, script), "start_running"},
	}
	for _, p := range paths {
		remonitor := strings.Index(p.body, "remonitor_running")
		if remonitor < 0 {
			t.Errorf("%s never hands the daemons back to monit", p.name)
			continue
		}
		if start := strings.Index(p.body, p.starts); start < 0 || remonitor < start {
			t.Errorf("%s restores monit before it starts the daemons itself", p.name)
		}
		if notify := strings.Index(p.body, "write_notify"); notify < 0 || remonitor < notify {
			t.Errorf("%s restores monit before it commits the status", p.name)
		}
	}
}

// A reader that catches the lock truncated but not yet written parses no PID,
// deletes the lock as stale, and the running update ends up unlocked - free
// for a second update to start and wipe the shared files/ directory.
func TestGenerateScript_PublishesThePIDByRename(t *testing.T) {
	s := &Service{updateDir: t.TempDir()}
	writeTestManifest(t, s)
	script, err := s.generateScript(validOpts())
	if err != nil {
		t.Fatalf("generateScript() error = %v", err)
	}

	if strings.Contains(script, `echo $$ > "$LOCK_FILE"`) {
		t.Error("the PID is written straight into the lock file, truncating it first")
	}
	if !strings.Contains(script, `mv -f "$LOCK_FILE.new" "$LOCK_FILE"`) {
		t.Error("the script must publish the new PID with a rename")
	}
}

// BusyBox sh on Asuswrt-Merlin has no "command" builtin: `command -v pgrep`
// exits 127 whether or not pgrep is installed. A guard written that way refuses
// every update on the very router it was meant to protect, and a monit gate
// written that way silently never unmonitors anything. Nothing in the generated
// script may depend on it.
func TestGenerateScript_DoesNotUseTheCommandBuiltin(t *testing.T) {
	s := &Service{updateDir: t.TempDir()}
	writeTestManifest(t, s)
	script, err := s.generateScript(validOpts())
	if err != nil {
		t.Fatalf("generateScript() error = %v", err)
	}

	// Comments are free to name the trap they explain; only what the shell
	// executes is pinned.
	for i, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if strings.Contains(line, "command -v") {
			t.Errorf("line %d probes with `command -v`, which BusyBox sh does not have; use have_cmd: %s", i+1, line)
		}
	}
	if !strings.Contains(script, "have_cmd() {") {
		t.Error("script must define have_cmd to probe for a command")
	}
	for _, cmd := range []string{"pgrep", "monit"} {
		if !strings.Contains(script, "have_cmd "+cmd) {
			t.Errorf("script must probe for %s through have_cmd", cmd)
		}
	}
}

func TestGenerateScript_RefusesWithoutPgrep(t *testing.T) {
	s := &Service{updateDir: t.TempDir()}
	writeTestManifest(t, s)
	script, err := s.generateScript(validOpts())
	if err != nil {
		t.Fatalf("generateScript() error = %v", err)
	}

	// pgrep decides both what gets stopped and what gets started again. When
	// it is missing its 127 reads as "this daemon is not running" inside
	// every `if` below, so nothing is stopped, cp -f lands under live
	// processes and notify.json still says ok.
	steps := afterOnExit(t, script)
	const guard = "if ! have_cmd pgrep; then"
	i := strings.Index(steps, guard)
	if i < 0 {
		t.Fatal("script must refuse to run without pgrep, not read its absence as \"nothing is running\"")
	}
	if j := strings.Index(steps, `pgrep -f "$bin"`); j >= 0 && j < i {
		t.Error("the pgrep guard must come before the first pgrep use")
	}

	block := steps[i:]
	if k := strings.Index(block, "\nfi\n"); k >= 0 {
		block = block[:k]
	}
	if !strings.Contains(block, "exit 1") {
		t.Error("the pgrep guard must abort the update, not just log and carry on")
	}
}

func TestGenerateScript_ReportsAFailedStart(t *testing.T) {
	s := &Service{updateDir: t.TempDir()}
	writeTestManifest(t, s)
	script, err := s.generateScript(validOpts())
	if err != nil {
		t.Fatalf("generateScript() error = %v", err)
	}

	// Non-bot daemons start before notify.json is committed as ok, so a
	// failed Web UI start can still rewrite it. Both needles have a copy
	// inside the trap, so this looks below it only.
	steps := afterOnExit(t, script)
	if !strings.Contains(steps, `if ! start_except "$NOTIFY_INIT"; then`) {
		t.Error("the happy path must notice a failed start instead of reporting the update complete")
	}
	if !strings.Contains(steps, "write_notify failed") {
		t.Error("a daemon that fails to come back must turn notify.json to failed")
	}

	body := functionBody(t, script, "start_except")
	if strings.Contains(body, "start || log") {
		t.Error("start_running must not swallow a failed start behind || log")
	}
	if !strings.Contains(body, "return") {
		t.Error("start_running must return a status its caller can branch on")
	}
}

// functionBody returns the body of a shell function, so an assertion lands on
// the path that function is on and not on the script mentioning the words
// somewhere.
func functionBody(t *testing.T, script, name string) string {
	t.Helper()

	open := name + "() {\n"
	i := strings.Index(script, open)
	if i < 0 {
		t.Fatalf("script has no %s function", name)
	}
	body := script[i+len(open):]
	j := strings.Index(body, "\n}\n")
	if j < 0 {
		t.Fatalf("%s is never closed", name)
	}
	return body[:j]
}

// onExitBody returns the body of the on_exit handler, so a recovery
// assertion is about the recovery path and not about the script mentioning
// the words somewhere.
func onExitBody(t *testing.T, script string) string {
	t.Helper()

	return functionBody(t, script, "on_exit")
}

// afterOnExit returns everything below the EXIT trap: the numbered steps the
// script walks on the happy path. Asserting there keeps a happy-path claim
// from being satisfied by the recovery copy of the same line.
func afterOnExit(t *testing.T, script string) string {
	t.Helper()

	const marker = "\ntrap on_exit EXIT\n"
	i := strings.Index(script, marker)
	if i < 0 {
		t.Fatal("script does not install on_exit as its EXIT trap")
	}
	return script[i+len(marker):]
}

func TestGenerateScript_NotifyFormat(t *testing.T) {
	s := &Service{updateDir: t.TempDir()}
	writeTestManifest(t, s)
	script, err := s.generateScript(RunOptions{
		OldVersion: "v1.2.0", NewVersion: "v1.3.0", ChatID: 0, Initiator: "webui",
	})
	if err != nil {
		t.Fatalf("generateScript() error = %v", err)
	}

	const want = `{"chat_id":$CHAT_ID,"old_version":"$OLD_VERSION","new_version":"$NEW_VERSION","status":"$1","initiator":"$INITIATOR"}`
	if !strings.Contains(script, want) {
		t.Errorf("notify.json template line missing or changed, want %s", want)
	}
}

// Note: this test is only as strict as the developer's /bin/sh. On a box where
// /bin/sh is dash (a good ash proxy) it catches bashisms; where /bin/sh is bash
// it does not. The golden test is the real contract; this one is a cheap extra.
func TestGenerateScript_IsValidShell(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}

	s := &Service{updateDir: t.TempDir()}
	writeTestManifest(t, s)
	script, err := s.generateScript(validOpts())
	if err != nil {
		t.Fatalf("generateScript() error = %v", err)
	}

	path := filepath.Join(t.TempDir(), "update.sh")
	if err := os.WriteFile(path, []byte(script), 0644); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command(sh, "-n", path).CombinedOutput()
	if err != nil {
		t.Fatalf("generated script has a syntax error: %v\n%s", err, out)
	}
}

func TestGenerateScript_Golden(t *testing.T) {
	// Default paths keep the render deterministic, so the golden file shows
	// the exact script that ships to routers - save for its FILES table,
	// which belongs to the payload and here comes from testManifest.
	// Regenerate with:
	//   UPDATE_GOLDEN=1 go test ./internal/updater -run TestGenerateScript_Golden -count=1
	s := &Service{updateDir: t.TempDir()}
	writeTestManifest(t, s)
	script, err := s.generateScript(RunOptions{
		OldVersion: "v1.2.0", NewVersion: "v1.3.0", ChatID: 42, Initiator: "bot",
	})
	if err != nil {
		t.Fatalf("generateScript() error = %v", err)
	}

	script = strings.ReplaceAll(script, s.getUpdateDir(), UpdateDir)
	golden := filepath.Join("testdata", "update_script.golden.sh")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll("testdata", 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(script), 0644); err != nil {
			t.Fatal(err)
		}
		t.Logf("rewrote %s", golden)
		return
	}

	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden: %v (regenerate with UPDATE_GOLDEN=1)", err)
	}
	if string(want) != script {
		t.Errorf("generated script differs from %s; review the diff and regenerate with UPDATE_GOLDEN=1 if the change is intended", golden)
	}
}

func TestRunUpdateScript_InvalidVersion(t *testing.T) {
	s := noExecService(t)

	tests := []struct {
		name    string
		mutate  func(*RunOptions)
		wantErr string
	}{
		{
			name:    "shell injection in old version",
			mutate:  func(o *RunOptions) { o.OldVersion = "v1.0.0;rm -rf /" },
			wantErr: "invalid old version",
		},
		{
			name:    "shell injection in new version",
			mutate:  func(o *RunOptions) { o.NewVersion = "v1.1.0$(whoami)" },
			wantErr: "invalid new version",
		},
		{
			name:    "backticks in old version",
			mutate:  func(o *RunOptions) { o.OldVersion = "`id`" },
			wantErr: "invalid old version",
		},
		{
			name:    "quotes in new version",
			mutate:  func(o *RunOptions) { o.NewVersion = `v1.1.0"test` },
			wantErr: "invalid new version",
		},
		{
			name:    "empty old version",
			mutate:  func(o *RunOptions) { o.OldVersion = "" },
			wantErr: "invalid old version",
		},
		{
			name:    "too long version",
			mutate:  func(o *RunOptions) { o.NewVersion = strings.Repeat("v", 100) },
			wantErr: "invalid new version",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := validOpts()
			tt.mutate(&opts)

			err := s.RunUpdateScript(opts)
			if err == nil {
				t.Error("Expected error for invalid version")
				return
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Error %q should contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestRunUpdateScript_InvalidInitiator(t *testing.T) {
	// Initiator is interpolated into the shell script the same way versions
	// are, so it gets the same allow-list treatment.
	s := noExecService(t)

	for _, initiator := range []string{"", "cron", `bot";rm -rf /;"`} {
		t.Run(initiator, func(t *testing.T) {
			opts := validOpts()
			opts.Initiator = initiator
			err := s.RunUpdateScript(opts)
			if err == nil || !strings.Contains(err.Error(), "invalid initiator") {
				t.Fatalf("RunUpdateScript(initiator=%q) error = %v, want invalid initiator", initiator, err)
			}
		})
	}
}

func TestRunUpdateScript_ValidVersion(t *testing.T) {
	s := noExecService(t)
	writeTestManifest(t, s)

	// The exec is expected to fail - the interpreter does not exist. A
	// validation error is not: it would mean valid versions were rejected.
	err := s.RunUpdateScript(validOpts())

	if err != nil && strings.Contains(err.Error(), "invalid") {
		t.Errorf("Valid versions should pass validation: %v", err)
	}
	assertExecRefused(t, s, err)

	// Check that script file was created
	if _, err := os.Stat(s.getScriptFile()); os.IsNotExist(err) {
		t.Error("Script file should be created")
	}
}

func TestRunUpdateScript_ScriptContent(t *testing.T) {
	s := noExecService(t)
	writeTestManifest(t, s)

	// The exec fails, but the script is written before it is launched
	err := s.RunUpdateScript(RunOptions{ChatID: 42, OldVersion: "v1.2.3", NewVersion: "v2.0.0", Initiator: "bot"})
	assertExecRefused(t, s, err)

	// Read the generated script
	content, err := os.ReadFile(s.getScriptFile())
	if err != nil {
		t.Fatalf("Failed to read script: %v", err)
	}

	script := string(content)

	// Verify script has correct shebang
	if !strings.HasPrefix(script, "#!/bin/sh") {
		t.Error("Script should start with #!/bin/sh")
	}

	// Verify embedded values
	if !strings.Contains(script, "CHAT_ID=42") {
		t.Error("Script missing correct CHAT_ID")
	}
	if !strings.Contains(script, `OLD_VERSION="v1.2.3"`) {
		t.Error("Script missing correct OLD_VERSION")
	}
	if !strings.Contains(script, `NEW_VERSION="v2.0.0"`) {
		t.Error("Script missing correct NEW_VERSION")
	}
}

// The bot deletes the whole update directory the moment it has reported a
// successful update - while this script is still on its last steps. Started
// inside that directory, the script is left with a working directory that no
// longer exists, and so is every daemon it starts. On the router monit then
// refuses to run at all ("Monit: Cannot read current directory"), so step 7's
// re-monitor is swallowed by its own `2>/dev/null || true` and the bot stays
// unmonitored; and every shell the daemons spawn prints "shell-init: error
// retrieving current directory" into whatever the Web UI is showing.
func TestUpdateCommand_StartsTheScriptOutsideTheDirectoryTheUpdateDeletes(t *testing.T) {
	updateDir := t.TempDir()
	s := &Service{updateDir: updateDir, shell: "/bin/sh"}

	cwdFile := filepath.Join(t.TempDir(), "cwd")
	probe := filepath.Join(updateDir, "probe.sh")
	if err := os.WriteFile(probe, []byte("pwd > "+cwdFile+"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	out, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()

	if err := s.updateCommand(probe, out).Run(); err != nil {
		t.Fatalf("run the probe script: %v", err)
	}

	data, err := os.ReadFile(cwdFile)
	if err != nil {
		t.Fatalf("read the probe's working directory: %v", err)
	}
	got := strings.TrimSpace(string(data))
	doomed, err := filepath.EvalSymlinks(updateDir)
	if err != nil {
		t.Fatal(err)
	}
	if got == doomed || strings.HasPrefix(got, doomed+string(os.PathSeparator)) {
		t.Errorf("the script runs from %q, inside the directory the update deletes", got)
	}
}

// The same deletion takes update.log with it, so every log call after that
// point writes into a directory that is gone. Under set -e a failing redirect
// ends the script where it stands - taking the monit re-monitor and the lock
// removal of steps 7 and 8 with it.
func TestGenerateScript_LogSurvivesTheDirectoryTheBotDeletes(t *testing.T) {
	s := &Service{updateDir: t.TempDir()}
	writeTestManifest(t, s)
	script, err := s.generateScript(validOpts())
	if err != nil {
		t.Fatalf("generateScript() error = %v", err)
	}

	logFn := regexp.MustCompile(`(?ms)^log\(\) \{.*?^\}`).FindString(script)
	if logFn == "" {
		t.Fatal("no log() definition in the generated script")
	}

	gone := filepath.Join(t.TempDir(), "removed", "update.log")
	snippet := "set -e\n" + logFn + "\nLOG_FILE=" + gone + "\nlog 'a line'\necho SURVIVED\n"
	out, err := exec.Command("/bin/sh", "-c", snippet).CombinedOutput()
	if err != nil {
		t.Fatalf("the script dies once its log file is gone: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "SURVIVED") {
		t.Errorf("output %q, want the shell to carry on past the failed log", out)
	}
}

// A daemon a release adds is not running before its first update - nothing
// ran it - and restarting only the daemons that ran would leave it stopped.
// Step 1 notes every daemon whose binary is absent, and once the copy has
// succeeded those join the success-start list: step 6 starts them before the
// bot. A daemon whose binary is there but stopped stays stopped.
func TestGenerateScript_StartsADaemonNewWithTheRelease(t *testing.T) {
	s := &Service{updateDir: t.TempDir()}
	writeTestManifest(t, s)
	script, err := s.generateScript(validOpts())
	if err != nil {
		t.Fatalf("generateScript() error = %v", err)
	}
	steps := afterOnExit(t, script)

	record := strings.Index(steps, `elif [ ! -e "$bin" ] && [ ! -L "$bin" ]; then`)
	copyFiles := strings.Index(steps, "# 4. Copy files")
	join := strings.Index(steps, `START_INITS="$RUNNING_INITS$NEW_INITS"`)
	start := strings.Index(steps, `if ! start_except "$NOTIFY_INIT"; then`)
	if record < 0 || copyFiles < 0 || join < 0 || start < 0 {
		t.Fatalf("missing a step: record %d, copy %d, join %d, start %d", record, copyFiles, join, start)
	}
	if !(record < copyFiles && copyFiles < join && join < start) {
		t.Errorf("the new daemons must be noted before the copy and join the running ones after it, before step 6")
	}
	if !strings.Contains(steps, `NEW_INITS="$NEW_INITS $init"`) {
		t.Error("step 1 does not note a daemon whose binary is absent")
	}
	if strings.Contains(onExitBody(t, script), "NEW_INITS") {
		t.Error("a failed update must not start a daemon it may not have installed")
	}
}

func TestGenerateScript_NewDaemonRecoverySandbox(t *testing.T) {
	bot, watchd, webui := "telegram-bot", "vpn-director-watchd", "webui"
	tests := []struct {
		name        string
		failure     string
		missing     []string
		running     []string
		starts      []string
		afterStart  bool
		stopFailure bool
	}{
		{name: "new daemon starts before the bot", missing: []string{watchd}, running: []string{bot, webui}, starts: []string{webui, watchd, bot}},
		{name: "existing stopped webui stays stopped", missing: []string{watchd}, running: []string{bot}, starts: []string{watchd, bot}},
		{name: "existing stopped watchd stays stopped", running: []string{bot}, starts: []string{bot}},
		{name: "all daemons are new", missing: []string{bot, watchd, webui}, starts: []string{watchd, webui, bot}},
		{name: "before copying", failure: "before-copy", missing: []string{watchd}, running: []string{bot, webui}},
		{name: "partial binary copy", failure: "copy-new", missing: []string{watchd}, running: []string{bot, webui}},
		{name: "binary permissions", failure: "permissions", missing: []string{watchd}, running: []string{bot, webui}},
		{name: "after successful copy", failure: "after-copy", missing: []string{watchd}, running: []string{bot, webui}},
		{name: "new start fails after spawning", failure: "vpn-director-watchd-start", missing: []string{watchd}, running: []string{bot, webui}, afterStart: true},
		{name: "existing start fails before new start", failure: "webui-start", missing: []string{watchd}, running: []string{bot, webui}},
		{name: "webui start fails after new start", failure: "webui-start", missing: []string{watchd, webui}, running: []string{bot}, afterStart: true},
		{name: "notify write fails after new start", failure: "notify", missing: []string{watchd}, running: []string{bot, webui}, afterStart: true},
		{name: "bot start fails after new start", failure: "telegram-bot-start", missing: []string{watchd}, running: []string{bot, webui}, afterStart: true},
		{name: "lock removal fails after remonitoring", failure: "late-lock", missing: []string{watchd}, running: []string{bot, webui}, afterStart: true},
		{name: "failed new stop needs kill", failure: "webui-start", missing: []string{watchd, webui}, running: []string{bot}, afterStart: true, stopFailure: true},
		{name: "no original daemons and webui start fails", failure: "webui-start", missing: []string{bot, watchd, webui}, afterStart: true},
		{name: "no original daemons and bot start fails", failure: "telegram-bot-start", missing: []string{bot, watchd, webui}, afterStart: true},
		{name: "failure leaves existing stopped daemons alone", failure: "telegram-bot-start", running: []string{bot}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			s := &Service{updateDir: filepath.Join(root, "update"), platform: "merlin"}
			writeTestManifest(t, s)
			script, err := s.generateScript(validOpts())
			if err != nil {
				t.Fatal(err)
			}
			tools := filepath.Join(root, "tools")
			state := filepath.Join(root, "state")
			for _, dir := range []string{tools, state} {
				if err := os.MkdirAll(dir, 0755); err != nil {
					t.Fatal(err)
				}
			}
			write := func(path, body string, mode os.FileMode) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(body), mode); err != nil {
					t.Fatal(err)
				}
			}
			contains := func(list []string, name string) bool {
				for _, item := range list {
					if item == name {
						return true
					}
				}
				return false
			}
			inits := map[string]string{
				"S98telegram-bot": bot, "S98vpn-director-watchd": watchd, "S98vpn-director-webui": webui,
			}
			table := regexp.MustCompile(`(?m)^FILES="([^"]*)"$`).FindStringSubmatch(script)
			if table == nil {
				t.Fatal("generated script has no FILES table")
			}
			for _, entry := range strings.Fields(table[1]) {
				src := strings.Split(entry, "|")[0]
				body := "payload\n"
				if name, ok := inits[filepath.Base(src)]; ok {
					body = "#!/bin/sh\nname=" + name + "\n" + sandboxInit
					write(filepath.Join(root, src), body, 0755)
				}
				write(filepath.Join(s.getFilesDir(), src), body, 0644)
			}
			for _, name := range []string{bot, watchd, webui} {
				if !contains(tt.missing, name) {
					write(filepath.Join(root, "opt/vpn-director", name), "old "+name+"\n", 0755)
				}
				if contains(tt.running, name) {
					write(filepath.Join(state, name+".running"), "", 0644)
				}
				write(filepath.Join(s.getFilesDir(), name), "new "+name+"\n", 0644)
			}
			write(filepath.Join(tools, "pgrep"), sandboxPgrep, 0755)
			write(filepath.Join(tools, "pkill"), sandboxPkill, 0755)
			write(filepath.Join(tools, "monit"), sandboxMonit, 0755)
			for _, op := range []string{"cp", "chmod", "rm", "cat"} {
				write(filepath.Join(tools, op), sandboxFault, 0755)
			}

			// Rehome every router path and replace process tools before executing.
			script = regexp.MustCompile(`(?m)^PATH=.*$`).ReplaceAllString(script, "PATH="+tools+":/usr/bin:/bin")
			for _, prefix := range []string{"/opt/", "/jffs/", "/var/lock"} {
				script = strings.ReplaceAll(script, prefix, root+prefix)
			}
			if regexp.MustCompile(`(^|[" =|])/(opt|jffs|var/lock)(/|\b)`).MatchString(script) {
				t.Fatal("unrehomed router path in sandbox script")
			}
			scriptPath := filepath.Join(root, "update.sh")
			callsPath := filepath.Join(root, "calls")
			write(scriptPath, script, 0644)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "/bin/sh", scriptPath)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "SANDBOX_ROOT="+root, "SANDBOX_ROOT_PATTERN="+regexp.QuoteMeta(root), "STATE_DIR="+state,
				"FILES_DIR="+s.getFilesDir(), "LOCK_FILE="+filepath.Join(s.getUpdateDir(), "lock"),
				"CALLS_FILE="+callsPath, "FAIL_AT="+tt.failure,
				"STOP_NEW_FAIL="+map[bool]string{false: "0", true: "1"}[tt.stopFailure])
			cmd.WaitDelay = time.Second
			out, runErr := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("sandbox exceeded its deadline: %v\n%s", ctx.Err(), out)
			}
			calls, err := os.ReadFile(callsPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("sandbox calls:\n%s", calls)
			if (runErr != nil) != (tt.failure != "") {
				t.Errorf("script error = %v, failure = %q\n%s", runErr, tt.failure, out)
			}
			wantStatus := "ok"
			if tt.failure != "" {
				wantStatus = "failed"
			}
			data, err := os.ReadFile(filepath.Join(s.getUpdateDir(), "notify.json"))
			if err != nil {
				t.Fatal(err)
			}
			var notify struct{ Status string }
			if err := json.Unmarshal(data, &notify); err != nil || notify.Status != wantStatus {
				t.Errorf("notify = %s, error = %v, want %s", data, err, wantStatus)
			}
			if _, err := os.Stat(filepath.Join(s.getUpdateDir(), "lock")); !os.IsNotExist(err) {
				t.Errorf("update lock remains: %v", err)
			}
			for _, name := range []string{bot, watchd, webui} {
				wantRunning := contains(tt.running, name) || (tt.failure == "" && contains(tt.missing, name))
				if !contains(tt.running, name) && !contains(tt.missing, name) {
					if strings.Contains(string(calls), name+" start\n") || strings.Contains(string(calls), "monit monitor "+name+"\n") {
						t.Errorf("existing stopped daemon %s was started or remonitored", name)
					}
				}
				for _, suffix := range []string{".running", ".monitored"} {
					_, err := os.Stat(filepath.Join(state, name+suffix))
					if err != nil && !os.IsNotExist(err) {
						t.Fatal(err)
					}
					if got := err == nil; got != wantRunning {
						t.Errorf("%s%s present = %v, want %v", name, suffix, got, wantRunning)
					}
				}
			}
			if tt.failure == "" {
				var starts []string
				for _, line := range strings.Split(string(calls), "\n") {
					if strings.HasSuffix(line, " start") {
						starts = append(starts, strings.TrimSuffix(line, " start"))
					}
				}
				if strings.Join(starts, " ") != strings.Join(tt.starts, " ") {
					t.Errorf("start order = %v, want %v", starts, tt.starts)
				}
				for _, name := range []string{bot, watchd, webui} {
					path := filepath.Join(root, "opt/vpn-director", name)
					data, err := os.ReadFile(path)
					if err != nil || string(data) != "new "+name+"\n" {
						t.Errorf("installed %s = %q, error = %v", name, data, err)
					}
					info, err := os.Stat(path)
					if err != nil || info.Mode()&0111 == 0 {
						t.Errorf("installed %s is not executable: %v", name, err)
					}
				}
			} else {
				fault := strings.Index(string(calls), "FAIL "+tt.failure+"\n")
				if fault < 0 {
					t.Fatal("the injected failure was never reached")
				}
				for _, name := range tt.missing {
					if strings.Contains(string(calls)[fault:], name+" start\n") {
						t.Errorf("recovery started new daemon %s", name)
					}
				}
				for _, name := range tt.running {
					if !strings.Contains(string(calls)[fault:], name+" start\n") {
						t.Errorf("recovery did not restart original daemon %s", name)
					}
				}
				if tt.afterStart {
					start := strings.Index(string(calls), watchd+" start\n")
					stop := strings.Index(string(calls)[fault:], watchd+" stop\n")
					if start < 0 || start > fault || stop < 0 {
						t.Errorf("new daemon must start before the fault and stop during recovery: start=%d fault=%d stop=%d", start, fault, stop)
					}
				}
				if tt.stopFailure && !strings.Contains(string(calls), watchd+" kill\n") {
					t.Error("a failed new-daemon stop left its process running without a kill attempt")
				}
			}
		})
	}
}

const sandboxInit = `printf '%s %s\n' "$name" "$1" >> "$CALLS_FILE"
case "$1" in
    start)
        : > "$STATE_DIR/$name.running"
        if [ "$FAIL_AT" = "$name-start" ] && [ ! -e "$STATE_DIR/fault" ]; then
            : > "$STATE_DIR/fault"
            printf 'FAIL %s\n' "$FAIL_AT" >> "$CALLS_FILE"
            exit 71
        fi
        ;;
    stop)
        if [ "$name" = "vpn-director-watchd" ] && [ "$STOP_NEW_FAIL" = "1" ]; then
            exit 72
        fi
        /bin/rm -f "$STATE_DIR/$name.running"
        ;;
    *) exit 90 ;;
esac
`

const sandboxProcessPattern = `process_name="${process_pattern##*/}"
process_anchored=0
case "$process_pattern" in
    '^'*'([[:space:]]|$)')
        process_anchored=1
        process_name="${process_name%'([[:space:]]|$)'}"
        ;;
esac
case "$process_name" in
    telegram-bot|vpn-director-watchd|webui) ;;
    *) exit 91 ;;
esac
if [ "$process_anchored" = "1" ]; then
    [ "$process_pattern" = "^$SANDBOX_ROOT_PATTERN/opt/vpn-director/$process_name([[:space:]]|$)" ] || exit 91
else
    [ "$process_pattern" = "$SANDBOX_ROOT/opt/vpn-director/$process_name" ] || exit 91
fi
`

const sandboxPgrep = `#!/bin/sh
[ "$#" -eq 2 ] && [ "$1" = "-f" ] || exit 90
process_pattern=$2
` + sandboxProcessPattern + `[ -f "$STATE_DIR/$process_name.running" ]
`

const sandboxPkill = `#!/bin/sh
[ "$#" -eq 3 ] && [ "$1" = "-9" ] && [ "$2" = "-f" ] || exit 90
process_pattern=$3
` + sandboxProcessPattern + `printf '%s kill\n' "$process_name" >> "$CALLS_FILE"
/bin/rm -f "$STATE_DIR/$process_name.running"
`

const sandboxMonit = `#!/bin/sh
printf 'monit %s %s\n' "$1" "$2" >> "$CALLS_FILE"
case "$1" in
    monitor) : > "$STATE_DIR/$2.monitored" ;;
    unmonitor) /bin/rm -f "$STATE_DIR/$2.monitored" ;;
    *) exit 90 ;;
esac
`

const sandboxFault = `#!/bin/sh
fail_once() {
    if [ ! -e "$STATE_DIR/fault" ]; then
        : > "$STATE_DIR/fault"
        printf 'FAIL %s\n' "$FAIL_AT" >> "$CALLS_FILE"
        exit 71
    fi
}
op="${0##*/}"
case "$op:$FAIL_AT" in
    cp:before-copy) [ "${2-}" != "$FILES_DIR/opt/vpn-director/vpn-director.sh" ] || fail_once ;;
    cp:copy-new) [ "${2-}" != "$FILES_DIR/vpn-director-watchd" ] || fail_once ;;
    chmod:permissions) [ "${2-}" != "$SANDBOX_ROOT/opt/vpn-director/vpn-director-watchd" ] || fail_once ;;
    rm:after-copy) [ "${1-}" != "-rf" ] || [ "${2-}" != "$FILES_DIR" ] || fail_once ;;
    rm:late-lock) [ "${1-}" != "-f" ] || [ "${2-}" != "$LOCK_FILE" ] || fail_once ;;
    cat:notify) [ ! -e "$STATE_DIR/vpn-director-watchd.running" ] || fail_once ;;
esac
exec "/bin/$op" "$@"
`

func TestGenerateScript_FirstInstallationSurvivesAFailedAttempt(t *testing.T) {
	tests := []struct {
		name    string
		failure string
		mode    string
		options firstInstallOptions
	}{
		{name: "partial new binary copy", failure: "partial-copy"},
		{name: "binary chmod", failure: "permissions"},
		{name: "after successful copy", failure: "after-copy"},
		{name: "init spawned before failure", failure: "vpn-director-watchd-start"},
		{name: "notify after new start", failure: "notify"},
		{name: "bot start after new start", failure: "telegram-bot-start"},
		{name: "late failure after remonitor", failure: "late-lock"},
		{name: "all new and webui start fails", failure: "webui-start", options: firstInstallOptions{allNew: true}},
		{name: "exit observed after kill", failure: "vpn-director-watchd-start", mode: "delayed-exit"},
		{name: "existing stopped watchd", failure: "permissions", options: firstInstallOptions{watchdExists: true}},
		{name: "another existing stopped daemon", failure: "after-copy", options: firstInstallOptions{webuiStopped: true}},
		{name: "existing dangling symlink", failure: "after-copy", options: firstInstallOptions{danglingWatchd: true}},
		{name: "dangling symlink on success", options: firstInstallOptions{danglingWatchd: true}},
		{name: "copy never reached", failure: "before-copy"},
		{name: "old binary copy fails before new copy", failure: "copy-old"},
		{name: "running daemon with absent binary", failure: "after-copy", options: firstInstallOptions{missingRunningBot: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newFirstInstallSandbox(t, tt.options)
			first := s.run(tt.failure, tt.mode)
			fault := strings.Index(first.calls, "FAIL "+tt.failure+"\n")
			if tt.failure != "" {
				if fault < 0 {
					t.Fatal("first attempt did not reach the injected failure")
				}
				for _, name := range s.originalRunning {
					if !strings.Contains(first.calls[fault:], name+" start\n") {
						t.Errorf("failure recovery did not restart original daemon %s", name)
					}
				}
				for _, name := range s.newDaemons {
					if strings.Contains(first.calls[fault:], name+" start\n") {
						t.Errorf("failure recovery started new daemon %s", name)
					}
				}
			}
			for _, name := range []string{DaemonBot, DaemonWatchd, DaemonWebUI} {
				wantRunning := s.contains(s.originalRunning, name) || (tt.failure == "" && s.contains(s.newDaemons, name))
				s.assertState(name, wantRunning)
				if !s.contains(s.newDaemons, name) {
					if _, err := os.Lstat(s.binary(name)); err != nil {
						t.Errorf("pre-existing or original-running binary %s disappeared: %v", name, err)
					}
					if strings.Contains(first.calls, "unlink "+name+"\n") {
						t.Errorf("cleanup tried to unlink unowned binary %s", name)
					}
				} else if tt.failure != "" {
					if _, err := os.Lstat(s.binary(name)); !os.IsNotExist(err) {
						t.Errorf("failed first installation left %s present: %v", name, err)
					}
				}
			}
			if tt.failure == "before-copy" || tt.failure == "copy-old" {
				if strings.Contains(first.calls, "unlink "+DaemonWatchd+"\n") {
					t.Error("cleanup acquired ownership without reaching the new binary copy")
				}
			} else if tt.failure != "" && s.contains(s.newDaemons, DaemonWatchd) {
				unlink := strings.Index(first.calls, "unlink "+DaemonWatchd+"\n")
				release := strings.Index(first.calls, "update lock released\n")
				if unlink < fault || release < unlink {
					t.Errorf("owned first copy must be removed after failure and before lock release: fault=%d unlink=%d release=%d", fault, unlink, release)
				}
				for _, name := range s.originalRunning {
					if restart := strings.Index(first.calls[fault:], name+" start\n"); restart >= 0 && fault+restart < unlink {
						t.Errorf("original daemon %s restarted before owned first-copy cleanup", name)
					}
				}
			}
			if strings.Contains(first.log, "manual recovery required") {
				t.Error("safe first-copy cleanup was reported as incomplete")
			}
			if tt.mode == "delayed-exit" && strings.Count(first.calls, "exit wait\n") != 2 {
				t.Errorf("cleanup did not observe delayed exit: %s", first.calls)
			}
			if tt.options.danglingWatchd {
				s.assertDanglingLinkPreserved()
			}
			s.assertIndependentFiles()

			// Only payload and fault controls are reset; installed files and daemon state persist.
			retry := s.run("", "")
			watchdNew := s.contains(s.newDaemons, DaemonWatchd)
			if got := strings.Count(retry.calls, DaemonWatchd+" start\n"); got != map[bool]int{false: 0, true: 1}[watchdNew] {
				t.Errorf("retry starts watchd %d times, new = %v", got, watchdNew)
			}
			if watchdNew {
				start := strings.Index(retry.calls, DaemonWatchd+" start\n")
				bot := strings.Index(retry.calls, DaemonBot+" start\n")
				monitor := strings.Index(retry.calls, "monit monitor "+DaemonWatchd+"\n")
				if start < 0 || bot < start || monitor < bot {
					t.Errorf("retry must start watchd once before bot, then remonitor: %s", retry.calls)
				}
			}
			for _, name := range []string{DaemonBot, DaemonWatchd, DaemonWebUI} {
				wantRunning := s.contains(s.originalRunning, name) || s.contains(s.newDaemons, name)
				s.assertState(name, wantRunning)
				if !wantRunning && (strings.Contains(first.calls, name+" start\n") || strings.Contains(retry.calls, name+" start\n")) {
					t.Errorf("an existing stopped daemon %s was started", name)
				}
				data, err := os.ReadFile(s.binary(name))
				if err != nil || string(data) != "new "+name+"\n" {
					t.Errorf("retry installed %s = %q, error = %v", name, data, err)
				}
				info, err := os.Stat(s.binary(name))
				if err != nil || info.Mode()&0111 == 0 {
					t.Errorf("retry binary %s is not executable: %v", name, err)
				}
			}
			if tt.options.danglingWatchd {
				s.assertDanglingLinkPreserved()
			}
			s.assertIndependentFiles()
		})
	}
}

func TestGenerateScript_FirstCopyCleanupRefusesUnsafeRemoval(t *testing.T) {
	for _, mode := range []string{"unlink-fails", "unlink-noop", "live", "lookup-error", "directory", "replacement-symlink"} {
		t.Run(mode, func(t *testing.T) {
			s := newFirstInstallSandbox(t, firstInstallOptions{})
			failure := "after-copy"
			if mode == "live" || mode == "lookup-error" {
				failure = "vpn-director-watchd-start"
			}
			result := s.run(failure, mode)
			if !strings.Contains(result.log, "first-install cleanup incomplete") || !strings.Contains(result.out, "manual recovery required") {
				t.Errorf("refused cleanup needs an explicit diagnostic, not retry-safe success:\nlog=%s\nout=%s", result.log, result.out)
			}
			if _, err := os.Lstat(s.binary(DaemonWatchd)); err != nil {
				t.Errorf("unsafe destination was deleted: %v", err)
			}
			if mode != "unlink-fails" && mode != "unlink-noop" && strings.Contains(result.calls, "unlink "+DaemonWatchd+"\n") {
				t.Error("cleanup attempted an unsafe unlink")
			}
			s.assertState(DaemonWatchd, mode == "live")
			if mode == "live" {
				if _, err := os.Stat(filepath.Join(s.state, DaemonWatchd+".monitored")); !os.IsNotExist(err) {
					t.Errorf("live but failed new daemon was remonitored: %v", err)
				}
				if waits := strings.Count(result.calls, "exit wait\n"); waits < 1 || waits > 5 {
					t.Errorf("live cleanup wait is not bounded: %d", waits)
				}
			}
			fault := strings.Index(result.calls, "FAIL "+failure+"\n")
			for _, name := range s.originalRunning {
				s.assertState(name, true)
				if fault < 0 || !strings.Contains(result.calls[fault:], name+" start\n") {
					t.Errorf("refused cleanup prevented original recovery for %s", name)
				}
				if strings.Contains(result.calls, "unlink "+name+"\n") {
					t.Errorf("refused cleanup touched original binary %s", name)
				}
			}
			if mode == "directory" {
				data, err := os.ReadFile(filepath.Join(s.binary(DaemonWatchd), "keep"))
				if err != nil || string(data) != "do not delete\n" {
					t.Errorf("directory contents changed: %q (%v)", data, err)
				}
			}
			if mode == "replacement-symlink" {
				link, err := os.Readlink(s.binary(DaemonWatchd))
				if err != nil || link != filepath.Join(s.root, "independent", "keep") {
					t.Errorf("replacement symlink changed: %q (%v)", link, err)
				}
			}
			s.assertIndependentFiles()
		})
	}
}

type firstInstallOptions struct {
	watchdExists      bool
	webuiStopped      bool
	danglingWatchd    bool
	allNew            bool
	missingRunningBot bool
}

type firstInstallSandbox struct {
	t               *testing.T
	mode            string
	root            string
	state           string
	tools           string
	calls           string
	s               *Service
	originalRunning []string
	newDaemons      []string
	binaryPayload   map[string]string
	env             []string
}

type firstInstallResult struct{ calls, out, log string }

func newFirstInstallSandbox(t *testing.T, opts firstInstallOptions) *firstInstallSandbox {
	t.Helper()
	s := &firstInstallSandbox{t: t, root: t.TempDir()}
	s.state = filepath.Join(s.root, "state")
	s.tools = filepath.Join(s.root, "tools")
	s.calls = filepath.Join(s.root, "calls")
	s.s = &Service{updateDir: filepath.Join(s.root, "update"), platform: "merlin"}
	if err := os.MkdirAll(s.state, 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{DaemonBot, DaemonWatchd, DaemonWebUI} {
		exists := name != DaemonWatchd || opts.watchdExists || opts.danglingWatchd
		running := name != DaemonWatchd && !(name == DaemonWebUI && opts.webuiStopped)
		if opts.allNew {
			exists, running = false, false
		}
		if name == DaemonBot && opts.missingRunningBot {
			exists = false
		}
		if exists {
			s.write(s.binary(name), "old "+name+"\n", 0755)
		}
		if running {
			s.write(filepath.Join(s.state, name+".running"), "", 0644)
			s.write(filepath.Join(s.state, name+".monitored"), "", 0644)
			s.originalRunning = append(s.originalRunning, name)
		} else if !exists {
			s.newDaemons = append(s.newDaemons, name)
		}
	}
	s.write(filepath.Join(s.root, "independent", "keep"), "do not delete\n", 0644)
	if opts.danglingWatchd {
		if err := os.Remove(s.binary(DaemonWatchd)); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(s.root, "independent", "dangling"), s.binary(DaemonWatchd)); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range Daemons {
		body := strings.ReplaceAll(sandboxInit, "printf 'FAIL %s\\n' \"$FAIL_AT\" >> \"$CALLS_FILE\"\n", "printf 'FAIL %s\\n' \"$FAIL_AT\" >> \"$CALLS_FILE\"\n"+sandboxUnrelated)
		body = strings.Replace(body, "    stop)\n", "    stop)\n        if [ \"$name\" = \"vpn-director-watchd\" ] && { [ \"$CLEANUP_MODE\" = \"live\" ] || [ \"$CLEANUP_MODE\" = \"delayed-exit\" ]; }; then exit 72; fi\n", 1)
		s.write(filepath.Join(s.root, "opt/etc/init.d", d.InitScript), "#!/bin/sh\nname="+d.Name+"\n"+body, 0755)
	}
	s.write(filepath.Join(s.tools, "pgrep"), strings.Replace(sandboxPgrep, `[ -f "$STATE_DIR/$process_name.running" ]`, firstCopyPgrep, 1), 0755)
	s.write(filepath.Join(s.tools, "pkill"), strings.Replace(sandboxPkill, `/bin/rm -f "$STATE_DIR/$process_name.running"`, firstCopyPkill, 1), 0755)
	s.write(filepath.Join(s.tools, "monit"), sandboxMonit, 0755)
	fault := strings.ReplaceAll(sandboxFault, "printf 'FAIL %s\\n' \"$FAIL_AT\" >> \"$CALLS_FILE\"\n", "printf 'FAIL %s\\n' \"$FAIL_AT\" >> \"$CALLS_FILE\"\n"+sandboxUnrelated)
	fault = strings.Replace(fault, "op=\"${0##*/}\"\n", "op=\"${0##*/}\"\n"+firstCopyBeforeFault, 1)
	fault = strings.Replace(fault, `exec "/bin/$op" "$@"`, firstCopyAfterFault+`exec "/bin/$op" "$@"`, 1)
	for _, op := range []string{"cp", "chmod", "rm", "cat"} {
		s.write(filepath.Join(s.tools, op), fault, 0755)
	}
	s.write(filepath.Join(s.tools, "sleep"), "#!/bin/sh\nprintf 'exit wait\\n' >> \"$CALLS_FILE\"\nexec /bin/sleep 0\n", 0755)
	return s
}

func (s *firstInstallSandbox) write(path, body string, mode os.FileMode) {
	s.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		s.t.Fatal(err)
	}
}

func (s *firstInstallSandbox) binary(name string) string {
	return filepath.Join(s.root, "opt/vpn-director", name)
}

func (s *firstInstallSandbox) contains(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

func (s *firstInstallSandbox) assertState(name string, running bool) {
	s.t.Helper()
	for _, suffix := range []string{".running", ".monitored"} {
		_, err := os.Stat(filepath.Join(s.state, name+suffix))
		if err != nil && !os.IsNotExist(err) {
			s.t.Fatal(err)
		}
		want := running
		if suffix == ".monitored" {
			want = running && !(name == DaemonWatchd && s.mode == "live")
		}
		if got := err == nil; got != want {
			s.t.Errorf("%s%s present = %v, want %v", name, suffix, got, want)
		}
	}
}

func (s *firstInstallSandbox) assertDanglingLinkPreserved() {
	s.t.Helper()
	link, err := os.Readlink(s.binary(DaemonWatchd))
	if err != nil || link != filepath.Join(s.root, "independent", "dangling") {
		s.t.Errorf("pre-existing symlink changed: %q (%v)", link, err)
	}
}

func (s *firstInstallSandbox) assertIndependentFiles() {
	s.t.Helper()
	data, err := os.ReadFile(filepath.Join(s.root, "independent", "keep"))
	if err != nil || string(data) != "do not delete\n" {
		s.t.Errorf("payload-independent file changed: %q (%v)", data, err)
	}
	for _, d := range Daemons {
		if _, err := os.Stat(filepath.Join(s.root, "opt/etc/init.d", d.InitScript)); err != nil {
			s.t.Errorf("init script %s was removed: %v", d.InitScript, err)
		}
	}
}

func (s *firstInstallSandbox) run(failure, mode string) firstInstallResult {
	s.t.Helper()
	if err := os.RemoveAll(s.s.getFilesDir()); err != nil {
		s.t.Fatal(err)
	}
	writeTestManifest(s.t, s.s)
	script, err := s.s.generateScript(validOpts())
	if err != nil {
		s.t.Fatal(err)
	}
	table := regexp.MustCompile(`(?m)^FILES="([^"]*)"$`).FindStringSubmatch(script)
	if table == nil {
		s.t.Fatal("generated script has no FILES table")
	}
	for _, entry := range strings.Fields(table[1]) {
		src := strings.Split(entry, "|")[0]
		body := "payload\n"
		for _, d := range Daemons {
			if filepath.Base(src) == d.InitScript {
				data, err := os.ReadFile(filepath.Join(s.root, "opt/etc/init.d", d.InitScript))
				if err != nil {
					s.t.Fatal(err)
				}
				body = string(data)
			}
		}
		s.write(filepath.Join(s.s.getFilesDir(), src), body, 0644)
	}
	for _, d := range Daemons {
		body := "new " + d.Name + "\n"
		if payload, ok := s.binaryPayload[d.Name]; ok {
			body = payload
		}
		s.write(filepath.Join(s.s.getFilesDir(), d.Name), body, 0644)
	}
	if err := os.Remove(filepath.Join(s.state, "fault")); err != nil && !os.IsNotExist(err) {
		s.t.Fatal(err)
	}
	s.mode = mode
	s.write(s.calls, "", 0644)
	script = regexp.MustCompile(`(?m)^PATH=.*$`).ReplaceAllString(script, "PATH="+s.tools+":/usr/bin:/bin")
	for _, prefix := range []string{"/opt/", "/jffs/", "/var/lock"} {
		script = strings.ReplaceAll(script, prefix, s.root+prefix)
	}
	if regexp.MustCompile(`(^|[" =|])/(opt|jffs|var/lock)(/|\b)`).MatchString(script) {
		s.t.Fatal("unrehomed router path in same-tree sandbox script")
	}
	path := filepath.Join(s.root, "update.sh")
	s.write(path, script, 0644)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", path)
	cmd.Dir = s.root
	cmd.Env = append(os.Environ(), "SANDBOX_ROOT="+s.root, "SANDBOX_ROOT_PATTERN="+regexp.QuoteMeta(s.root), "STATE_DIR="+s.state,
		"FILES_DIR="+s.s.getFilesDir(), "LOCK_FILE="+filepath.Join(s.s.getUpdateDir(), "lock"),
		"CALLS_FILE="+s.calls, "FAIL_AT="+failure, "STOP_NEW_FAIL=0", "CLEANUP_MODE="+mode)
	cmd.Env = append(cmd.Env, s.env...)
	cmd.WaitDelay = time.Second
	out, runErr := cmd.CombinedOutput()
	if ctx.Err() != nil {
		s.t.Fatalf("same-tree sandbox exceeded deadline: %v\n%s", ctx.Err(), out)
	}
	if (runErr != nil) != (failure != "") {
		s.t.Errorf("attempt failure=%q error=%v\n%s", failure, runErr, out)
	}
	data, err := os.ReadFile(filepath.Join(s.s.getUpdateDir(), "notify.json"))
	if err != nil {
		s.t.Fatal(err)
	}
	var notify struct{ Status string }
	want := "ok"
	if failure != "" {
		want = "failed"
	}
	if err := json.Unmarshal(data, &notify); err != nil || notify.Status != want {
		s.t.Errorf("notify=%s error=%v, want %s", data, err, want)
	}
	if _, err := os.Stat(filepath.Join(s.s.getUpdateDir(), "lock")); !os.IsNotExist(err) {
		s.t.Errorf("attempt did not release update lock: %v", err)
	}
	calls, err := os.ReadFile(s.calls)
	if err != nil {
		s.t.Fatal(err)
	}
	log, err := os.ReadFile(filepath.Join(s.s.getUpdateDir(), "update.log"))
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Logf("failure=%q mode=%q calls:\n%s\noutput:\n%s", failure, mode, calls, out)
	return firstInstallResult{calls: string(calls), out: string(out), log: string(log)}
}

const firstCopyPgrep = `if [ "$process_name" = "vpn-director-watchd" ] && [ "$CLEANUP_MODE" = "discovery-error" ]; then
    if [ ! -e "$STATE_DIR/fault" ]; then
        : > "$STATE_DIR/fault"
        printf 'FAIL discovery-error\n' >> "$CALLS_FILE"
    fi
    exit 2
fi
if [ "$process_name" = "vpn-director-watchd" ] && [ -e "$STATE_DIR/fault" ]; then
    if [ "$CLEANUP_MODE" = "lookup-error" ]; then exit 2; fi
    if [ "$CLEANUP_MODE" = "delayed-exit" ] && [ -e "$STATE_DIR/kill-pending" ]; then
        left=$(/bin/cat "$STATE_DIR/kill-pending")
        if [ "$left" -eq 0 ]; then
            /bin/rm -f "$STATE_DIR/kill-pending" "$STATE_DIR/vpn-director-watchd.running"
        else
            printf '%s\n' "$((left - 1))" > "$STATE_DIR/kill-pending"
        fi
    fi
fi
[ -f "$STATE_DIR/$process_name.running" ] && exit 0
if [ "$process_name" = "vpn-director-watchd" ] && [ -f "$STATE_DIR/unrelated.cmdline" ]; then
    exec /bin/grep -E "$process_pattern" "$STATE_DIR/unrelated.cmdline"
fi
exit 1`

const sandboxUnrelated = `if [ "${CLEANUP_MODE-}" = "unrelated" ]; then
    printf '%s/helper --mentions %s/opt/vpn-director/vpn-director-watchd\n' "$SANDBOX_ROOT" "$SANDBOX_ROOT" > "$STATE_DIR/unrelated.cmdline"
fi
`

const firstCopyPkill = `if [ "$process_name" = "vpn-director-watchd" ]; then
    if [ "$CLEANUP_MODE" = "live" ]; then exit 1; fi
    if [ "$CLEANUP_MODE" = "delayed-exit" ]; then
        printf '2\n' > "$STATE_DIR/kill-pending"
        exit 0
    fi
fi
if [ "$process_name" = "vpn-director-watchd" ] && [ -f "$STATE_DIR/unrelated.cmdline" ]; then
    if /bin/grep -E "$process_pattern" "$STATE_DIR/unrelated.cmdline" >/dev/null; then
        printf 'unrelated killed\n' >> "$CALLS_FILE"
        /bin/rm -f "$STATE_DIR/unrelated.cmdline"
    fi
fi
/bin/rm -f "$STATE_DIR/$process_name.running"`

const firstCopyBeforeFault = `if [ "$op" = "cp" ]; then
    case "${3-}" in
        "$SANDBOX_ROOT"/opt/vpn-director/telegram-bot|"$SANDBOX_ROOT"/opt/vpn-director/vpn-director-watchd|"$SANDBOX_ROOT"/opt/vpn-director/webui)
            printf 'copy %s\n' "${3##*/}" >> "$CALLS_FILE"
            ;;
    esac
    if [ "$FAIL_AT" = "partial-copy" ] && [ "${3-}" = "$SANDBOX_ROOT/opt/vpn-director/vpn-director-watchd" ] && [ ! -e "$STATE_DIR/fault" ]; then
        printf 'partial binary\n' > "$3"
        fail_once
    fi
    if [ "$FAIL_AT" = "copy-old" ] && [ "${3-}" = "$SANDBOX_ROOT/opt/vpn-director/telegram-bot" ]; then fail_once; fi
fi
`

const firstCopyAfterFault = `if [ "$op" = "cp" ] && [ "${3-}" = "$SANDBOX_ROOT/opt/vpn-director/vpn-director-watchd" ]; then
    if [ -L "$3" ]; then
        /bin/cat "$2" > "$3" || exit 74
    else
        /bin/cp "$@" || exit 74
    fi
    if [ "$CLEANUP_MODE" = "directory" ]; then
        /bin/rm -f "$3"
        /bin/mkdir -p "$3"
        printf 'do not delete\n' > "$3/keep"
    elif [ "$CLEANUP_MODE" = "replacement-symlink" ]; then
        /bin/rm -f "$3"
        /bin/ln -s "$SANDBOX_ROOT/independent/keep" "$3"
    fi
    exit 0
fi
if [ "$op" = "rm" ] && [ "${1-}" = "-f" ]; then
    case "${2-}" in
        "$SANDBOX_ROOT"/opt/vpn-director/telegram-bot|"$SANDBOX_ROOT"/opt/vpn-director/vpn-director-watchd|"$SANDBOX_ROOT"/opt/vpn-director/webui)
            printf 'unlink %s\n' "${2##*/}" >> "$CALLS_FILE"
            if [ "$CLEANUP_MODE" = "unlink-fails" ]; then exit 73; fi
            if [ "$CLEANUP_MODE" = "unlink-noop" ]; then exit 0; fi
            ;;
    esac
    if [ "${2-}" = "$LOCK_FILE" ]; then
        /bin/rm "$@" || exit 73
        printf 'update lock released\n' >> "$CALLS_FILE"
        exit 0
    fi
fi
`

func TestGenerateScript_FirstCopyDiscoveryNeedsRunningEvidence(t *testing.T) {
	s := newFirstInstallSandbox(t, firstInstallOptions{})
	result := s.run("discovery-error", "discovery-error")
	if !strings.Contains(result.log, "cannot determine whether vpn-director-watchd is running") {
		t.Errorf("unknown running state was treated as first-copy ownership: %s", result.log)
	}
	if strings.Contains(result.calls, "copy ") || strings.Contains(result.calls, "unlink ") {
		t.Errorf("unknown running state must refuse before copying or unlinking: %s", result.calls)
	}
	if _, err := os.Lstat(s.binary(DaemonWatchd)); !os.IsNotExist(err) {
		t.Errorf("unknown ownership created a binary: %v", err)
	}
	for _, name := range s.originalRunning {
		s.assertState(name, true)
	}
	s.assertState(DaemonWatchd, false)
	s.assertIndependentFiles()
}

func TestGenerateScript_NewCleanupIgnoresUnrelatedMentions(t *testing.T) {
	for _, failure := range []string{"after-copy", "vpn-director-watchd-start"} {
		t.Run(failure, func(t *testing.T) {
			s := newFirstInstallSandbox(t, firstInstallOptions{})
			result := s.run(failure, "unrelated")
			mention := filepath.Join(s.state, "unrelated.cmdline")
			want := s.root + "/helper --mentions " + s.binary(DaemonWatchd) + "\n"
			if data, err := os.ReadFile(mention); err != nil || string(data) != want {
				t.Errorf("new cleanup killed an unrelated process mention: %q (%v)", data, err)
			}
			if strings.Contains(result.calls, "unrelated killed\n") || strings.Contains(result.calls, DaemonWatchd+" kill\n") {
				t.Error("an unrelated mention caused the cleanup kill path")
			}
			if _, err := os.Lstat(s.binary(DaemonWatchd)); !os.IsNotExist(err) {
				t.Errorf("unrelated mention blocked owned first-copy removal: %v", err)
			}
			if strings.Contains(result.log, "manual recovery required") || strings.Contains(result.out, "manual recovery required") || strings.Contains(result.calls, "exit wait\n") {
				t.Error("cleanup waited/refused while only an unrelated mention remained")
			}
			for _, name := range s.originalRunning {
				s.assertState(name, true)
			}
			s.assertState(DaemonWatchd, false)
			s.assertIndependentFiles()
			// End this owned unrelated fixture before retry: legacy discovery is unchanged.
			if err := os.Remove(mention); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			retry := s.run("", "")
			if strings.Count(retry.calls, DaemonWatchd+" start\n") != 1 || strings.Contains(retry.log, "manual recovery required") {
				t.Error("successful retry lost the first-install start/cleanup contract")
			}
			s.assertState(DaemonWatchd, true)
			s.assertIndependentFiles()
		})
	}
}

func cleanupScript(t *testing.T) string {
	t.Helper()
	s := &Service{updateDir: t.TempDir()}
	writeTestManifest(t, s)
	script, err := s.generateScript(validOpts())
	if err != nil {
		t.Fatal(err)
	}
	return script
}

func cleanupPattern(t *testing.T, script, binary string) string {
	t.Helper()
	body := functionBody(t, script, "daemon_argv0_pattern")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "set -e\ndaemon_argv0_pattern() {\n"+body+"\n}\ndaemon_argv0_pattern \"$1\"\n", "owned-pattern", binary)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("POSIX process-pattern helper failed: %v\n%s", err, out)
	}
	pattern := strings.TrimSuffix(string(out), "\n")
	if pattern != "^"+regexp.QuoteMeta(binary)+"([[:space:]]|$)" {
		t.Fatalf("unsafe/inexact argv[0] pattern %q for %q; real process tools refused", pattern, binary)
	}
	return pattern
}

func TestGenerateScript_CleanupPatternHasLiteralArgvZeroSemantics(t *testing.T) {
	script := cleanupScript(t)
	paths := []string{"/owned/plain/daemon", `/owned/a.b[c]d(e)f{g}h+i*j?k^l$m|n\\daemon`}
	for _, char := range `\\.^$*+?()[]{}|` {
		paths = append(paths, "/owned/dir"+string(char)+"literal/daemon")
	}
	for _, binary := range paths {
		t.Run(binary, func(t *testing.T) {
			pattern := cleanupPattern(t, script, binary)
			re, err := regexp.CompilePOSIX(pattern)
			if err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				line string
				want bool
			}{
				{binary, true}, {binary + " --dev", true}, {binary + "\t--dev", true},
				{"/owned/unrelated --mentions " + binary, false}, {"sh " + binary, false},
				{"prefix" + binary, false}, {binary + "-extra", false}, {binary + "child --dev", false},
				{strings.Replace(binary, "/owned/", "/owned-other/", 1), false},
			} {
				if got := re.MatchString(tc.line); got != tc.want {
					t.Errorf("pattern matched %q=%v, want %v", tc.line, got, tc.want)
				}
			}
			if strings.Contains(binary, ".") {
				near := strings.Replace(binary, ".", "x", 1)
				if re.MatchString(near) {
					t.Errorf("ERE metacharacter matched a near path: %q", near)
				}
			}
		})
	}
}

func TestProcessToolSandboxRetainsStrictPatternGuards(t *testing.T) {
	s := newFirstInstallSandbox(t, firstInstallOptions{})
	binary := s.binary(DaemonWatchd)
	anchored := "^" + regexp.QuoteMeta(binary) + "([[:space:]]|$)"
	for _, tool := range []string{"pgrep", "pkill"} {
		for _, pattern := range []string{"/unowned/vpn-director-watchd", "sh " + binary, binary + "-extra", anchored + ".*", "^" + binary + "$", binary + "/../foreign"} {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			args := []string{"-f", pattern}
			if tool == "pkill" {
				args = append([]string{"-9"}, args...)
			}
			cmd := exec.CommandContext(ctx, filepath.Join(s.tools, tool), args...)
			cmd.Env = append(os.Environ(), "SANDBOX_ROOT="+s.root, "SANDBOX_ROOT_PATTERN="+regexp.QuoteMeta(s.root), "STATE_DIR="+s.state, "CALLS_FILE="+s.calls, "CLEANUP_MODE=")
			out, err := cmd.CombinedOutput()
			cancel()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 91 {
				t.Errorf("%s accepted unowned/malformed pattern %q: %v %s", tool, pattern, err, out)
			}
		}
	}
}

// Re-exec copies of this test ELF are finite synthetic daemons, including a bare argv[0].
func init() {
	if os.Getenv("VPD_UPDATER_OWNED_PROCESS") == "1" {
		fmt.Println("READY")
		time.Sleep(20 * time.Second)
		os.Exit(0)
	}
}

type ownedCleanupProcess struct {
	cmd   *exec.Cmd
	start string
	group string
	argv0 string
	done  chan error
}

func ownedCleanupIdentity(pid int) ([]string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(string(data)[strings.LastIndex(string(data), ")")+2:])
	if len(fields) < 20 {
		return nil, errors.New("short owned process stat")
	}
	return fields, nil
}

func (p *ownedCleanupProcess) verify(t *testing.T) {
	t.Helper()
	fields, err := ownedCleanupIdentity(p.cmd.Process.Pid)
	if err != nil || fields[19] != p.start || fields[2] != p.group || fields[0] == "Z" {
		t.Fatalf("owned process identity/liveness changed: pid=%d err=%v", p.cmd.Process.Pid, err)
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", p.cmd.Process.Pid))
	if err != nil || string(bytes.Split(data, []byte{0})[0]) != p.argv0 {
		t.Fatalf("owned process argv[0] changed: %v", err)
	}
}

func startCleanupProcess(t *testing.T, executable, argv0 string, args []string, group int) *ownedCleanupProcess {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Args[0] = argv0
	cmd.Env = append(os.Environ(), "VPD_UPDATER_OWNED_PROCESS=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: group}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	fields, err := ownedCleanupIdentity(cmd.Process.Pid)
	if err != nil {
		cancel()
		cmd.Wait()
		t.Fatal(err)
	}
	p := &ownedCleanupProcess{cmd: cmd, start: fields[19], group: fields[2], argv0: argv0, done: make(chan error, 1)}
	ready := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(stdout).ReadString('\n')
		if err == nil && line != "READY\n" {
			err = fmt.Errorf("unexpected ready line: %q", line)
		}
		ready <- err
	}()
	select {
	case err := <-ready:
		if err != nil {
			cancel()
			cmd.Wait()
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		cancel()
		cmd.Wait()
		<-ready
		t.Fatal("owned synthetic process did not become ready")
	}
	go func() { p.done <- cmd.Wait() }()
	t.Cleanup(func() {
		select {
		case <-p.done:
			cancel()
			return
		default:
		}
		p.verify(t)
		cancel()
		select {
		case <-p.done:
		case <-time.After(3 * time.Second):
			t.Error("owned process was not reaped")
		}
	})
	return p
}

func TestGenerateScript_CleanupMatchesAndKillsOnlyOwnedArgvZero(t *testing.T) {
	pgrep, err := exec.LookPath("pgrep")
	if err != nil {
		t.Skip("procps pgrep unavailable")
	}
	pkill, err := exec.LookPath("pkill")
	if err != nil {
		t.Skip("procps pkill unavailable")
	}
	script := cleanupScript(t)
	for _, withArgs := range []bool{false, true} {
		t.Run(fmt.Sprint(withArgs), func(t *testing.T) {
			root := t.TempDir()
			binary := filepath.Join(root, `daemon.[x]+(core)^$?*{n}\\`)
			pattern := cleanupPattern(t, script, binary) // Refuse unsafe real tools before starting any child.
			data, err := os.ReadFile(os.Args[0])
			if err != nil {
				t.Fatal(err)
			}
			paths := []string{binary, binary + "-suffix", strings.Replace(binary, "daemon.", "daemonx", 1), filepath.Join(root, "unrelated")}
			for _, path := range paths {
				if err := os.WriteFile(path, data, 0700); err != nil {
					t.Fatal(err)
				}
			}
			var args []string
			if withArgs {
				args = []string{"--owned-argument"}
			}
			genuine := startCleanupProcess(t, binary, binary, args, 0)
			group := genuine.cmd.Process.Pid
			others := []*ownedCleanupProcess{
				startCleanupProcess(t, paths[3], paths[3], []string{"--mentions", binary}, group),
				startCleanupProcess(t, paths[1], paths[1], nil, group),
				startCleanupProcess(t, paths[2], paths[2], nil, group),
				startCleanupProcess(t, paths[3], "prefix"+binary, nil, group),
			}
			for _, p := range append(others, genuine) {
				p.verify(t)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, pgrep, "-g", strconv.Itoa(group), "-f", pattern).CombinedOutput()
			if err != nil || strings.TrimSpace(string(out)) != strconv.Itoa(genuine.cmd.Process.Pid) {
				t.Fatalf("real procps matched more than the owned daemon: %v %s", err, out)
			}
			// Guard wrappers constrain real procps to the verified owned group and exact safe pattern.
			tools := filepath.Join(root, "tools")
			if err := os.Mkdir(tools, 0700); err != nil {
				t.Fatal(err)
			}
			for _, tool := range []struct{ name, path, args string }{{"pgrep", pgrep, "-f"}, {"pkill", pkill, "-9 -f"}} {
				body := "#!/bin/sh\n[ \"$*\" = \"" + tool.args + " $SAFE_PATTERN\" ] || exit 93\nprintf '%s %s\\n' \"${0##*/}\" \"$*\" >> \"$TOOL_CALLS\"\n"
				if tool.name == "pgrep" {
					body += "exec \"$REAL_PGREP\" -g \"$OWNED_GROUP\" -f \"$SAFE_PATTERN\"\n"
				} else {
					body += "exec \"$REAL_PKILL\" -9 -g \"$OWNED_GROUP\" -f \"$SAFE_PATTERN\"\n"
				}
				if err := os.WriteFile(filepath.Join(tools, tool.name), []byte(body), 0700); err != nil {
					t.Fatal(err)
				}
			}
			inits := filepath.Join(root, "inits")
			if err := os.Mkdir(inits, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(inits, "S-owned"), []byte("#!/bin/sh\nexit 72\n"), 0700); err != nil {
				t.Fatal(err)
			}
			calls := filepath.Join(root, "process-calls")
			env := append(os.Environ(), "SAFE_PATTERN="+pattern, "OWNED_GROUP="+strconv.Itoa(group), "REAL_PGREP="+pgrep, "REAL_PKILL="+pkill, "TOOL_CALLS="+calls, "PATH="+tools+":/usr/bin:/bin", "INIT_DIR="+inits, "DAEMONS=owned|"+binary+"|S-owned")
			functions := "daemon_argv0_pattern() {\n" + functionBody(t, script, "daemon_argv0_pattern") + "\n}\nstop_new() {\n" + functionBody(t, script, "stop_new") + "\n}\n"
			snippet := functions + "have_cmd() { return 1; }\nlog() { :; }\nfirst_copy_error() { printf '%s\\n' \"$*\" >&2; }\nSTARTED_NEW_INITS=S-owned\nstop_new\n"
			cmd := exec.CommandContext(ctx, "/bin/sh", "-c", snippet)
			cmd.Env = env
			out, err = cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("production stop_new failed in owned procps fixture: %v %s", err, out)
			}
			select {
			case err := <-genuine.done:
				var exit *exec.ExitError
				if !errors.As(err, &exit) || genuine.cmd.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
					t.Fatalf("genuine daemon was not stopped/reaped by SIGKILL: %v", err)
				}
				// Keep the cleanup's completed-process receipt available.
				genuine.done <- err
			case <-time.After(3 * time.Second):
				t.Fatal("production stop_new did not kill the genuine owned daemon")
			}
			for _, p := range others {
				p.verify(t)
			}
			snippet = "daemon_argv0_pattern() {\n" + functionBody(t, script, "daemon_argv0_pattern") + "\n}\nfirst_copy_stopped() {\n" + functionBody(t, script, "first_copy_stopped") + "\n}\nfirst_copy_error() { printf '%s\\n' \"$*\" >&2; }\nfirst_copy_stopped \"$1\"\n"
			cmd = exec.CommandContext(ctx, "/bin/sh", "-c", snippet, "owned-cleanup", binary)
			cmd.Env = env
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("unrelated mentions blocked real first-copy confirmation: %v %s", err, out)
			}
			for _, p := range others {
				p.verify(t)
			}
			actualCalls, err := os.ReadFile(calls)
			if err != nil || strings.Count(string(actualCalls), "pkill -9 -f "+pattern+"\n") != 1 || strings.Count(string(actualCalls), "pgrep -f "+pattern+"\n") < 2 {
				t.Fatalf("new cleanup sites did not share the exact safe process pattern: %v %s", err, actualCalls)
			}
			t.Logf("procps owned group=%d genuine=%d stopped/reaped; survivor pids=%v; calls:\n%s", group, genuine.cmd.Process.Pid, []int{others[0].cmd.Process.Pid, others[1].cmd.Process.Pid, others[2].cmd.Process.Pid, others[3].cmd.Process.Pid}, actualCalls)
		})
	}
}
