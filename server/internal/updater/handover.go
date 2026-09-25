package updater

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// Limits step 1 holds step 2 to: item 6 of the contract (selfupdate.go).
const (
	// defaultHandoverTimeout bounds step 2: downloading the release and
	// starting its script. The Web UI waits up to twenty minutes for an update.
	defaultHandoverTimeout = 15 * time.Minute
	// maxProgressLines and maxProgressRunes cap what step 2 can put in front
	// of the user, so a faulty step 2 cannot flood a chat. The rest is logged.
	maxProgressLines = 10
	maxProgressRunes = 300
	// maxPendingLine bounds a line still waiting for its newline.
	maxPendingLine = 64 * 1024
	// defaultInstallerWaitDelay bounds the wait for step 2's output once it
	// has exited or been killed.
	defaultInstallerWaitDelay = 5 * time.Second
	// installerStartAttempts retries an exec refused with "text file busy": a
	// process forked elsewhere in this daemon while the installer was still
	// open for writing holds it until that process execs (golang/go#22315).
	installerStartAttempts = 5
	installerStartBackoff  = 100 * time.Millisecond
)

// HandoverPhase tells where a handover failed, so the caller can phrase it.
type HandoverPhase int

const (
	// PhaseDownload: the new version's binary could not be downloaded.
	PhaseDownload HandoverPhase = iota + 1
	// PhaseStart: the new version's binary did not start.
	PhaseStart
	// PhaseInstaller: the new version's step 2 exited non-zero.
	PhaseInstaller
	// PhaseTimeout: step 2 ran out of time and was killed.
	PhaseTimeout
)

// HandoverError is a handover that did not get the update script started.
type HandoverError struct {
	Phase HandoverPhase
	Err   error
}

func (e *HandoverError) Error() string {
	switch e.Phase {
	case PhaseDownload:
		return "download the new version: " + e.Err.Error()
	case PhaseStart:
		return "start the new version: " + e.Err.Error()
	case PhaseTimeout:
		return "the new version timed out: " + e.Err.Error()
	default:
		return "the new version failed: " + e.Err.Error()
	}
}

func (e *HandoverError) Unwrap() error { return e.Err }

// Handover is step 1 of a self-update (selfupdate.go). It downloads this
// daemon's binary of release as the installer, runs its self-update step with
// opts and passes each line that step prints on to progress. It returns nil
// once step 2 reports the update script started; the script then owns the lock
// and files/. After a failure it removes files/ and the lock, as long as the
// lock still names this process.
func (s *Service) Handover(ctx context.Context, release *Release, opts RunOptions, progress func(string)) error {
	if progress == nil {
		progress = func(string) {}
	}
	installer := s.getInstallerFile()
	// Needed by nobody afterwards: on success step 2 has linked itself into
	// files/, and after a failure nothing runs it again. A failure that still
	// owns the directory takes it away in cleanUpOwned, under the claim; this
	// covers the success path and a lock that has passed to the update script.
	defer os.Remove(installer)

	if err := s.downloadInstaller(ctx, release, installer); err != nil {
		s.cleanUpOwned()
		return &HandoverError{Phase: PhaseDownload, Err: err}
	}
	if err := s.runInstaller(ctx, installer, opts, progress); err != nil {
		s.cleanUpOwned()
		return err
	}
	return nil
}

// installerAsset picks the binary step 1 runs: this daemon's, else the first
// other daemon's the release carries. Every one of them implements step 2.
// The fallback cannot finish an update on its own today: step 2 requires every
// binary of its Daemons table and fails in DownloadRelease on the missing one.
// It is there for a later release that drops a daemon, whose step 2 no longer
// asks for it.
func (s *Service) installerAsset(release *Release) (name, url string, err error) {
	suffix, err := s.getArchSuffix()
	if err != nil {
		return "", "", err
	}
	names := []string{s.daemon}
	for _, d := range Daemons {
		if d.Name != s.daemon {
			names = append(names, d.Name)
		}
	}
	for _, n := range names {
		if n == "" {
			continue
		}
		if u := assetURL(release, n+"-"+suffix); u != "" {
			return n + "-" + suffix, u, nil
		}
	}
	return "", "", fmt.Errorf("release %s carries no daemon binary for %s", release.TagName, suffix)
}

// downloadInstaller downloads the installer under a temporary name and renames
// it into place once it is complete, closed and executable.
func (s *Service) downloadInstaller(ctx context.Context, release *Release, installer string) error {
	name, url, err := s.installerAsset(release)
	if err != nil {
		return err
	}
	if err := requireHTTPS(url); err != nil {
		return fmt.Errorf("asset %s: %w", name, err)
	}
	part := installer + ".part"
	if err := s.downloadAsset(ctx, url, part); err != nil {
		os.Remove(part)
		return fmt.Errorf("%s: %w", name, err)
	}
	if err := os.Chmod(part, 0755); err != nil {
		os.Remove(part)
		return fmt.Errorf("%s: %w", name, err)
	}
	if err := os.Rename(part, installer); err != nil {
		os.Remove(part)
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// runInstaller runs step 2 and waits for it, at most the handover timeout.
// Its stdout lines go to progress; its last stderr line is the reason a
// failure is reported with - its first crash line, when it died of a Go
// runtime failure.
func (s *Service) runInstaller(ctx context.Context, installer string, opts RunOptions, progress func(string)) error {
	ctx, cancel := context.WithTimeout(ctx, s.getHandoverTimeout())
	defer cancel()

	var reason, crash string
	stdout := &lineWriter{line: progressRelay(progress)}
	stderr := &lineWriter{line: func(l string) {
		slog.Warn("self-update step 2", "stderr", l)
		reason = l
		// A Go runtime failure - a panic, or the out-of-memory kill a 256 MB
		// router makes plausible - ends stderr with stack frames, so the last
		// line would be a frame. The first "panic:" or "fatal error:" line is
		// the headline, and the one thing the user can act on.
		if crash == "" && (strings.HasPrefix(l, "panic:") || strings.HasPrefix(l, "fatal error:")) {
			crash = l
		}
	}}

	cmd, err := startInstaller(ctx, installer, selfUpdateArgv(opts, s.platform), stdout, stderr, s.getInstallerWaitDelay())
	if err != nil {
		return &HandoverError{Phase: PhaseStart, Err: err}
	}
	err = cmd.Wait()
	stdout.flush()
	stderr.flush()
	if crash != "" {
		reason = crash
	}
	return handoverOutcome(cmd.ProcessState, ctx.Err(), err, reason)
}

// handoverOutcome decides what step 2 did from what it left behind: state is
// what Wait put in cmd.ProcessState, ctxErr the context's error by then,
// waitErr what Wait returned and reason step 2's last stderr line.
func handoverOutcome(state *os.ProcessState, ctxErr, waitErr error, reason string) error {
	// The exit status decides, not Wait's error: exit 0 means the script has
	// started and owns the lock and files/. Wait still returns an error for an
	// exit 0 when the deadline fired between that exit and the reap, or when
	// something step 2 started held its output open (ErrWaitDelay).
	if state != nil && state.Success() {
		return nil
	}
	// A timeout is what the deadline took from step 2 by force. A step 2 that
	// answered the SIGTERM with an exit code of its own had something to say,
	// and so did one whose context was cancelled rather than timed out:
	// reporting either as "Update timed out" throws away the reason, which is
	// the one thing the user can act on.
	if errors.Is(ctxErr, context.DeadlineExceeded) && endedBySignal(state) {
		return &HandoverError{Phase: PhaseTimeout, Err: ctxErr}
	}
	if reason == "" {
		// This branch is here for a step 2 that said nothing, so it may not
		// hand the user an empty message either: "Update failed:" with
		// nothing after it says less than the least this can say.
		reason = "no reason given"
		if waitErr != nil {
			reason = waitErr.Error()
		}
	}
	return &HandoverError{Phase: PhaseInstaller, Err: errors.New(truncateRunes(reason, maxProgressRunes))}
}

// endedBySignal reports whether step 2 was ended by a signal instead of
// choosing its own exit code. A process that left no state at all never got
// to choose one either.
func endedBySignal(state *os.ProcessState) bool {
	if state == nil {
		return true
	}
	ws, ok := state.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled()
}

// startInstaller starts step 2 from "/", retrying while exec reports
// "text file busy". Stdin stays nil, which os/exec connects to /dev/null.
func startInstaller(ctx context.Context, installer string, argv []string, stdout, stderr *lineWriter, waitDelay time.Duration) (*exec.Cmd, error) {
	for attempt := 1; ; attempt++ {
		cmd := exec.CommandContext(ctx, installer, argv...)
		cmd.Dir = "/"
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		// Ask before killing. A step 2 that has already started the update
		// script can catch this and exit 0, and the exit status is what
		// decides success, so the script keeps the lock and files/ instead of
		// having them wiped from under it. WaitDelay escalates to SIGKILL for
		// one that does not answer. No router can update the step 1 it runs,
		// so a later step 2 gets this chance only if it is given here.
		cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
		cmd.WaitDelay = waitDelay
		err := cmd.Start()
		if err == nil {
			return cmd, nil
		}
		if !errors.Is(err, syscall.ETXTBSY) || attempt == installerStartAttempts {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(installerStartBackoff):
		}
	}
}

// lineWriter hands every complete, non-empty line written to it to line,
// trimmed. A line that outgrows maxPendingLine is handed over in pieces.
type lineWriter struct {
	line    func(string)
	pending []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.pending = append(w.pending, p...)
	for {
		i := bytes.IndexByte(w.pending, '\n')
		if i < 0 {
			break
		}
		w.emit(w.pending[:i])
		w.pending = w.pending[i+1:]
	}
	if len(w.pending) > maxPendingLine {
		cut := completeRunes(w.pending)
		if cut == 0 {
			// Nothing whole to hand over: emit the buffer rather than let it
			// grow without bound waiting for a rune that may never end.
			cut = len(w.pending)
		}
		w.emit(w.pending[:cut])
		w.pending = w.pending[cut:]
	}
	return len(p), nil
}

// completeRunes returns where to cut b so that the piece does not end inside
// a rune still being written: before a trailing rune start that is not a
// whole rune yet, else len(b). Such a piece is invalid UTF-8, and the short
// tail of a split line reaches Telegram exactly as it is - only a piece over
// maxProgressRunes is laundered into U+FFFD on its way through truncateRunes -
// so Telegram refuses the message carrying it. Only the last UTFMax bytes can
// hold an unfinished rune; a b with no rune start in them at all is output
// already malformed, and is handed on as it stands rather than held back.
func completeRunes(b []byte) int {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if !utf8.RuneStart(b[i]) {
			continue
		}
		if utf8.FullRune(b[i:]) {
			return len(b)
		}
		return i
	}
	return len(b)
}

// flush hands over a last line that ended without a newline.
func (w *lineWriter) flush() {
	w.emit(w.pending)
	w.pending = nil
}

func (w *lineWriter) emit(b []byte) {
	if line := strings.TrimSpace(string(b)); line != "" {
		w.line(line)
	}
}

// progressRelay passes the first maxProgressLines lines on to progress, each
// cut to maxProgressRunes, and only logs the rest.
func progressRelay(progress func(string)) func(string) {
	sent := 0
	return func(line string) {
		if sent >= maxProgressLines {
			slog.Info("self-update step 2 progress past the limit", "line", line)
			return
		}
		sent++
		progress(truncateRunes(line, maxProgressRunes))
	}
}

// truncateRunes cuts s to at most n runes.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// cleanUpOwned removes the installer, files/ and the lock after a failed
// handover, but only while the lock names this process. A lock naming another
// process means the update script has taken over, and the directory is its to
// clean. The lock goes last, so the whole cleanup happens while the claim
// still says whose directory this is.
func (s *Service) cleanUpOwned() {
	if !s.lockNamesPID(os.Getpid()) {
		slog.Warn("the update lock names another process, leaving the update directory to it",
			"lock", s.getLockFile())
		return
	}
	os.Remove(s.getInstallerFile())
	s.CleanFiles()
	s.RemoveLock()
}
