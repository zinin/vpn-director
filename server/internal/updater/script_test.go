package updater

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
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
// reads; DownloadRelease writes it there in production. Only the golden test
// renders with the default paths - it is the one render that has to be
// deterministic - and so is the only one writing into the real
// /tmp/vpn-director-update, from which what lands there is taken back out
// afterwards.
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
	s := &Service{}
	writeTestManifest(t, s)
	script, err := s.generateScript(RunOptions{
		OldVersion: "v1.2.0", NewVersion: "v1.3.0", ChatID: 42, Initiator: "bot",
	})
	if err != nil {
		t.Fatalf("generateScript() error = %v", err)
	}

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

	record := strings.Index(steps, `elif [ ! -e "$bin" ]; then`)
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
			cmd.Env = append(os.Environ(), "SANDBOX_ROOT="+root, "STATE_DIR="+state,
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

const sandboxPgrep = `#!/bin/sh
[ "$#" -eq 2 ] && [ "$1" = "-f" ] || exit 90
case "$2" in
    "$SANDBOX_ROOT"/opt/vpn-director/*) ;;
    *) exit 91 ;;
esac
[ -f "$STATE_DIR/${2##*/}.running" ]
`

const sandboxPkill = `#!/bin/sh
[ "$#" -eq 3 ] && [ "$1" = "-9" ] && [ "$2" = "-f" ] || exit 90
case "$3" in
    "$SANDBOX_ROOT"/opt/vpn-director/*) ;;
    *) exit 91 ;;
esac
printf '%s kill\n' "${3##*/}" >> "$CALLS_FILE"
/bin/rm -f "$STATE_DIR/${3##*/}.running"
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
