package updater

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/platform"
)

func TestParseSelfUpdateArgs_AcceptsTheInvocationStep1Builds(t *testing.T) {
	argv := selfUpdateArgv(RunOptions{OldVersion: "v1.2.3", NewVersion: "v1.2.4", ChatID: 42, Initiator: "bot"}, "")
	if argv[0] != SelfUpdateCommand {
		t.Fatalf("argv[0] = %q, want %q", argv[0], SelfUpdateCommand)
	}
	got, err := parseSelfUpdateArgs(argv[1:])
	if err != nil {
		t.Fatalf("parseSelfUpdateArgs() error = %v", err)
	}
	if want := (selfUpdateArgs{From: "v1.2.3", To: "v1.2.4", Initiator: "bot", ChatID: 42}); got != want {
		t.Errorf("parsed %+v, want %+v", got, want)
	}
}

// Telegram group chats have negative IDs; step 1 passes them through.
func TestParseSelfUpdateArgs_AcceptsANegativeChatID(t *testing.T) {
	argv := selfUpdateArgv(RunOptions{OldVersion: "v1.2.3", NewVersion: "v1.2.4", ChatID: -1001234567890, Initiator: "bot"}, "")
	got, err := parseSelfUpdateArgs(argv[1:])
	if err != nil || got.ChatID != -1001234567890 {
		t.Fatalf("parsed %+v (%v), want chat id -1001234567890", got, err)
	}
}

// --platform is the one flag a released step 1 may not have passed, so its
// absence has to stay legal: step 2 then detects. A name no release knows is
// refused instead, since step 1 shows the last stderr line as the reason.
func TestParseSelfUpdateArgs_PlatformIsOptionalAndValidated(t *testing.T) {
	opts := RunOptions{OldVersion: "v1.2.3", NewVersion: "v1.2.4", ChatID: 42, Initiator: "bot"}

	got, err := parseSelfUpdateArgs(selfUpdateArgv(opts, "")[1:])
	if err != nil || got.Platform != "" {
		t.Fatalf("parsed %+v (%v), want an empty platform: step 2 detects then", got, err)
	}

	got, err = parseSelfUpdateArgs(selfUpdateArgv(opts, platform.Merlin)[1:])
	if err != nil || got.Platform != platform.Merlin {
		t.Fatalf("parsed %+v (%v), want platform %q", got, err, platform.Merlin)
	}

	if _, err := parseSelfUpdateArgs(selfUpdateArgv(opts, "plan9")[1:]); err == nil {
		t.Error("parseSelfUpdateArgs() accepted an unknown --platform")
	}
}

func TestParseSelfUpdateArgs_RefusesWhatTheScriptCannotTake(t *testing.T) {
	valid := []string{"--from", "v1.2.3", "--to", "v1.2.4", "--initiator", "webui", "--chat-id", "0"}
	with := func(name, value string) []string {
		out := append([]string(nil), valid...)
		for i := 0; i < len(out); i += 2 {
			if out[i] == name {
				out[i+1] = value
			}
		}
		return out
	}
	for name, args := range map[string][]string{
		"shell in --from":     with("--from", "v1.2.3;id"),
		"empty --to":          with("--to", ""),
		"unknown initiator":   with("--initiator", "cron"),
		"non-numeric chat id": with("--chat-id", "forty-two"),
		"unknown flag":        append(append([]string(nil), valid...), "--force"),
		"positional argument": append(append([]string(nil), valid...), "extra"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseSelfUpdateArgs(args); err == nil {
				t.Errorf("parseSelfUpdateArgs(%q) accepted it", args)
			}
		})
	}
}

// TestSelfUpdateArgv_EveryReleasedFormStillParses holds step 2 to the
// invocations released step 1s make. A router cannot update the step 1 it
// runs, so every line of the file is an invocation some router makes for as
// long as it exists. The file only grows.
func TestSelfUpdateArgv_EveryReleasedFormStillParses(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "selfupdate_argv.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var forms [][]string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		forms = append(forms, strings.Fields(line))
	}
	if len(forms) == 0 {
		t.Fatal("testdata/selfupdate_argv.txt lists no released invocation")
	}
	// The file's header fixes the options every line encodes, so parsing one
	// into the wrong fields fails here as surely as refusing it. The platform
	// is the header's "merlin" on the lines that carry the flag, and empty on
	// the older ones, which leave step 2 to detect it.
	for _, form := range forms {
		if form[0] != SelfUpdateCommand {
			t.Errorf("released form %q does not start with %q", strings.Join(form, " "), SelfUpdateCommand)
			continue
		}
		want := selfUpdateArgs{From: "v1.2.3", To: "v1.2.4", Initiator: "bot", ChatID: 42}
		if slices.Contains(form, "--platform") {
			want.Platform = platform.Merlin
		}
		got, err := parseSelfUpdateArgs(form[1:])
		if err != nil {
			t.Errorf("released form %q no longer parses: %v", strings.Join(form, " "), err)
			continue
		}
		if got != want {
			t.Errorf("released form %q parses to %+v, want %+v", strings.Join(form, " "), got, want)
		}
	}
	opts := RunOptions{OldVersion: "v1.2.3", NewVersion: "v1.2.4", ChatID: 42, Initiator: "bot"}
	got := strings.Join(selfUpdateArgv(opts, platform.Merlin), " ")
	if newest := strings.Join(forms[len(forms)-1], " "); got != newest {
		t.Errorf("step 1 builds %q, but the newest released form is %q: append the new form as a line, never edit one", got, newest)
	}
}

// step2Fixture is a step 2 with every path in a temp dir: it runs as the webui
// binary of v1.2.4, its parent (PID 4242) holds the lock, and its update script
// runs under an interpreter that does nothing.
type step2Fixture struct {
	s         *Service
	dir       string
	self      string
	requested func() []string
}

// newStep2Fixture serves the GitHub API and the raw files over TLS. onRequest,
// when set, sees every request path and the lock file before it is answered.
func newStep2Fixture(t *testing.T, onRequest func(path, lockFile string)) *step2Fixture {
	t.Helper()
	dir := t.TempDir()
	lock := filepath.Join(dir, "lock")
	const parent = 4242
	if err := os.WriteFile(lock, []byte(strconv.Itoa(parent)), 0644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var requested []string
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requested = append(requested, r.URL.Path)
		mu.Unlock()
		if onRequest != nil {
			onRequest(r.URL.Path, lock)
		}
		switch {
		case r.URL.Path == "/repos/zinin/vpn-director/releases/tags/v1.2.4":
			fmt.Fprintf(w, `{"tag_name": "v1.2.4", "assets": [
				{"url": "%[1]s/repos/zinin/vpn-director/releases/assets/301", "id": 301, "name": "telegram-bot-arm64",
				 "browser_download_url": "%[1]s/zinin/vpn-director/releases/download/v1.2.4/telegram-bot-arm64"},
				{"url": "%[1]s/repos/zinin/vpn-director/releases/assets/302", "id": 302, "name": "webui-arm64",
				 "browser_download_url": "%[1]s/zinin/vpn-director/releases/download/v1.2.4/webui-arm64"}]}`, server.URL)
		case strings.HasPrefix(r.URL.Path, "/repos/zinin/vpn-director/releases/assets/"):
			// The API sends the file from the CDN, /assets/ here, to a request
			// that asks for it, and describes the asset to any other.
			id := strings.TrimPrefix(r.URL.Path, "/repos/zinin/vpn-director/releases/assets/")
			if r.Header.Get("Accept") != "application/octet-stream" {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				fmt.Fprintf(w, `{"id": %s, "name": %q}`, id, fakeAssets[id])
				return
			}
			http.Redirect(w, r, "/assets/"+fakeAssets[id], http.StatusFound)
		case strings.HasSuffix(r.URL.Path, "/"+manifestPath):
			w.Write([]byte(testManifest))
		case strings.HasPrefix(r.URL.Path, "/assets/"):
			w.Write([]byte("downloaded " + strings.TrimPrefix(r.URL.Path, "/assets/")))
		default:
			w.Write([]byte("#!/bin/sh\n"))
		}
	}))
	t.Cleanup(server.Close)

	self := filepath.Join(dir, "installer")
	if err := os.WriteFile(self, []byte("the new webui"), 0755); err != nil {
		t.Fatal(err)
	}
	noop := filepath.Join(dir, "noop-sh")
	if err := os.WriteFile(noop, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	s := &Service{
		httpClient: server.Client(),
		baseURL:    server.URL,
		rawBaseURL: server.URL,
		updateDir:  dir,
		lockFile:   lock,
		scriptFile: filepath.Join(dir, "update.sh"),
		shell:      noop,
		archSuffix: "arm64",
		platform:   "merlin",
		daemon:     DaemonWebUI,
		parentPID:  func() int { return parent },
		executable: func() (string, error) { return self, nil },
	}
	return &step2Fixture{s: s, dir: dir, self: self, requested: func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), requested...)
	}}
}

// step2Args is what step 1 passes for an update from v1.2.3 to to, started
// from the Web UI.
func step2Args(to string) []string {
	return selfUpdateArgv(RunOptions{OldVersion: "v1.2.3", NewVersion: to, ChatID: 0, Initiator: "webui"}, "")[1:]
}

// The assertion here is the test binary surviving the SIGTERM the update
// script sends to it: without the signal.Notify in selfUpdate the whole
// package run dies at this point. Step 1 sends that signal when its timeout
// fires, and step 2 must live through it once the script is up.
func TestSelfUpdate_SurvivesStep1sSIGTERMOnceTheScriptHasStarted(t *testing.T) {
	f := newStep2Fixture(t, nil)
	// The script is started detached but stays this process's child, so its
	// $PPID is the test binary.
	marker := filepath.Join(f.dir, "script-ran")
	shell := filepath.Join(f.dir, "terminating-sh")
	body := "#!/bin/sh\nkill -TERM $PPID\necho started > '" + marker + "'\nexit 0\n"
	if err := os.WriteFile(shell, []byte(body), 0755); err != nil {
		t.Fatal(err)
	}
	f.s.shell = shell
	var stdout bytes.Buffer

	if err := f.s.selfUpdate(context.Background(), step2Args("v1.2.4"), "v1.2.4", &stdout); err != nil {
		t.Fatalf("selfUpdate() error = %v", err)
	}

	// The marker says the script really ran and really sent the signal; a test
	// that never got one would pass for the wrong reason.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the update script never ran, so nothing sent the SIGTERM this test is about")
		}
	}
}

// The daemons resolve the platform at startup, from --platform or detection,
// and step 2 must not decide it over again: on a router whose detection is
// wrong - the reason --platform exists - it would install another firmware's
// files. Step 1 passes what it resolved, and that beats the detection this
// environment answers (merlin, pinned for the package in TestMain).
func TestSelfUpdate_TakesThePlatformStep1PassedOverDetection(t *testing.T) {
	if p, err := platform.Detect(); err != nil || p.Name != platform.Merlin {
		t.Fatalf("detection answers %+v (%v), want %q - this test needs the two to differ", p, err, platform.Merlin)
	}
	f := newStep2Fixture(t, nil)
	f.s.platform = "" // nothing has told this step 2 anything yet
	args := selfUpdateArgv(RunOptions{OldVersion: "v1.2.3", NewVersion: "v1.2.4", ChatID: 0, Initiator: "webui"},
		platform.Keenetic)[1:]

	if err := f.s.selfUpdate(context.Background(), args, "v1.2.4", &bytes.Buffer{}); err != nil {
		t.Fatalf("selfUpdate() error = %v", err)
	}

	got, err := f.s.getPlatform()
	if err != nil {
		t.Fatalf("getPlatform() error = %v", err)
	}
	if got != platform.Keenetic {
		t.Errorf("getPlatform() = %q, want %q: the platform step 1 passed must win", got, platform.Keenetic)
	}
}

func TestSelfUpdate_InstallsItsOwnReleaseAndStartsTheScript(t *testing.T) {
	f := newStep2Fixture(t, nil)
	var stdout bytes.Buffer

	if err := f.s.selfUpdate(context.Background(), step2Args("v1.2.4"), "v1.2.4", &stdout); err != nil {
		t.Fatalf("selfUpdate() error = %v", err)
	}

	if got := stdout.String(); got != "Files downloaded, starting update...\n" {
		t.Errorf("stdout = %q, want the one progress line", got)
	}
	selfInfo, err := os.Stat(f.self)
	if err != nil {
		t.Fatal(err)
	}
	ownInfo, err := os.Stat(filepath.Join(f.dir, "files", DaemonWebUI))
	if err != nil || !os.SameFile(selfInfo, ownInfo) {
		t.Errorf("files/%s is not this binary (%v)", DaemonWebUI, err)
	}
	// The claim check in front of every write must not stand in the way of
	// the files the manifest lists.
	if _, err := os.Stat(filepath.Join(f.dir, "files", "opt", "vpn-director", "vpn-director.sh")); err != nil {
		t.Errorf("a file the manifest lists never landed in files/: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(f.dir, "files", DaemonBot)); err != nil || string(data) != "downloaded telegram-bot-arm64" {
		t.Errorf("files/%s = %q (%v), want the release asset", DaemonBot, data, err)
	}
	// The template is unchanged and pinned byte for byte by
	// TestGenerateScript_Golden; this run renders it with temp paths, so it
	// checks what this run fed into it.
	script, err := os.ReadFile(f.s.getScriptFile())
	if err != nil {
		t.Fatalf("no update script: %v", err)
	}
	for _, want := range []string{`OLD_VERSION="v1.2.3"`, `NEW_VERSION="v1.2.4"`, `INITIATOR="webui"`, "CHAT_ID=0"} {
		if !strings.Contains(string(script), want) {
			t.Errorf("update script lacks %s", want)
		}
	}
	for _, p := range f.requested() {
		if strings.Contains(p, "webui-arm64") {
			t.Errorf("step 2 downloaded %s, the binary it is", p)
		}
	}
}

func TestSelfUpdate_RefusesAReleaseOtherThanItsOwn(t *testing.T) {
	f := newStep2Fixture(t, nil)

	err := f.s.selfUpdate(context.Background(), step2Args("v1.2.4"), "v1.2.5", &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "v1.2.5") || !strings.Contains(err.Error(), "v1.2.4") {
		t.Fatalf("selfUpdate() error = %v, want a refusal naming both versions", err)
	}
	if got := f.requested(); len(got) != 0 {
		t.Errorf("a refused step 2 made requests: %v", got)
	}
}

func TestSelfUpdate_RefusesUnlessItsParentHoldsTheLock(t *testing.T) {
	f := newStep2Fixture(t, nil)
	f.s.parentPID = func() int { return 4343 }

	err := f.s.selfUpdate(context.Background(), step2Args("v1.2.4"), "v1.2.4", &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "lock") {
		t.Fatalf("selfUpdate() error = %v, want the lock refusal", err)
	}
	if got := f.requested(); len(got) != 0 {
		t.Errorf("step 2 without the lock made requests: %v", got)
	}
}

// The download can take minutes. A claim lost meanwhile - the parent died and
// another update took the directory - must not get the script started. This
// step 2 runs as the bot's binary, so the webui's asset is the last thing it
// writes and the claim goes once every write is done: the check in front of
// the script is the only one left to catch it.
func TestSelfUpdate_ChecksTheLockAgainBeforeTheScript(t *testing.T) {
	f := newStep2Fixture(t, func(path, lockFile string) {
		if strings.HasPrefix(path, "/assets/"+DaemonWebUI+"-") {
			os.WriteFile(lockFile, []byte("1"), 0644)
		}
	})
	// Which holds while the webui is the last daemon in the table: reorder it
	// and the claim would go before a write still to come, leaving one of the
	// per-write checks to refuse and this test covering something else.
	if last := Daemons[len(Daemons)-1]; last.Name != DaemonWebUI {
		t.Fatalf("Daemons ends with %s, so the webui's asset is not step 2's last write", last.Name)
	}
	f.s.daemon = DaemonBot

	err := f.s.selfUpdate(context.Background(), step2Args("v1.2.4"), "v1.2.4", &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "lock") {
		t.Fatalf("selfUpdate() error = %v, want the lock refusal", err)
	}
	sawRequest := func(substr string) bool {
		for _, p := range f.requested() {
			if strings.Contains(p, substr) {
				return true
			}
		}
		return false
	}
	// The whole download went through before the refusal, which is what tells
	// this check apart from the one step 2 makes before it starts.
	for _, want := range []string{manifestPath, "/assets/" + DaemonWebUI + "-"} {
		if !sawRequest(want) {
			t.Errorf("step 2 stopped before requesting %s, so an earlier check refused", want)
		}
	}
	if _, err := os.Stat(f.s.getScriptFile()); !os.IsNotExist(err) {
		t.Error("the update script was written for a claim that was lost")
	}
}

// Step 1 shows the user the last line step 2 prints on stderr and reads
// progress from stdout, so a refusal belongs on stderr, with exit status 1.
// Each case names what it is about in the refusal: an argv otherwise complete
// carries the one value under test, or the case would be refused for the
// missing rest of it and prove nothing.
func TestRunSelfUpdate_ReportsARefusalOnStderr(t *testing.T) {
	for name, tt := range map[string]struct {
		args []string
		want string
	}{
		"a --to the update script could not take": {
			args: []string{"--from", "v1.2.3", "--to", "v1.2.4;id", "--initiator", "webui", "--chat-id", "0"},
			want: "--to",
		},
		"another version": {args: step2Args("v1.0.0"), want: "v1.0.0"},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := RunSelfUpdate(tt.args, DaemonBot, "v1.2.4", &stdout, &stderr); code != 1 {
				t.Errorf("RunSelfUpdate() = %d, want 1", code)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want nothing: it is the progress channel", stdout.String())
			}
			if !strings.Contains(stderr.String(), tt.want) {
				t.Errorf("stderr = %q, want the reason to name %q", stderr.String(), tt.want)
			}
		})
	}
}

// The daemon running step 1 can die mid-handover - out of memory on a 256 MB
// router, now that a second Go process runs during the download, is the
// plausible way - and step 2 is re-parented and keeps downloading. A retry
// then shares files/ with it, and os.Create truncates what the retry has
// already fetched. Step 2 asks before each file whether the claim it was
// started under still stands; a parent that is gone fails that on its own,
// because os.Getppid answers 1 once it is.
func TestSelfUpdate_StopsWritingOnceTheClaimIsGone(t *testing.T) {
	for name, tt := range map[string]struct {
		lostAt    string   // the request served while the claim changes hands
		wantFiles []string // what files/ holds once step 2 has stopped
	}{
		// The file served while the claim changes hands is not published
		// either: it is staged beside its target and the claim is re-checked
		// before the rename, so the retry that took the directory over keeps
		// its own payload. Publishing it was the truncation the rule forbids.
		"before the files the manifest lists": {
			lostAt:    manifestPath,
			wantFiles: nil,
		},
		"between the daemon binaries": {
			lostAt:    "/assets/" + DaemonBot + "-",
			wantFiles: []string{"files.manifest", "jffs", "opt"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newStep2Fixture(t, func(path, lockFile string) {
				if strings.Contains(path, tt.lostAt) {
					os.WriteFile(lockFile, []byte("1"), 0644)
				}
			})

			err := f.s.selfUpdate(context.Background(), step2Args("v1.2.4"), "v1.2.4", &bytes.Buffer{})

			if !errors.Is(err, errClaimLost) {
				t.Fatalf("selfUpdate() error = %v, want the claim refusal", err)
			}
			// One refusal wherever in the download it comes from: the user is
			// told the claim is gone, not which loop noticed.
			if err.Error() != errClaimLost.Error() {
				t.Errorf("refusal = %q, want it worded like every other one: %q", err, errClaimLost)
			}
			entries, err := os.ReadDir(filepath.Join(f.dir, "files"))
			if err != nil {
				t.Fatal(err)
			}
			var written []string
			for _, e := range entries {
				written = append(written, e.Name())
			}
			if !slices.Equal(written, tt.wantFiles) {
				t.Errorf("files/ holds %v, want %v: step 2 wrote on past the claim", written, tt.wantFiles)
			}
		})
	}
}
