//go:build linux

package watchcompat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const compatibleJSON = `{"protocol_version":1,"watch_owner":"watchd"}`

func gateFixture(t *testing.T) *Gate {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "proc")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	return &Gate{BotPath: filepath.Join(dir, "vpn-director-bot"), ProcRoot: root}
}

func writeCapabilityExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"[ \"$#\" -eq 1 ] && [ \"$1\" = \"--watchd-capabilities\" ] || exit 91\n" +
		"[ \"$(pwd -P)\" = \"/\" ] || exit 92\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

func capabilityReply(payload string) string {
	return fmt.Sprintf("printf '%%s\\n' %s", shellQuote(payload))
}

func gateProcessStat(pid int, start int64) string {
	// starttime is field 22.
	return fmt.Sprintf("%d (vpn-director-bo) S 1 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 %d 0 0 0 0 0\n", pid, start)
}

func writeBotProcess(t *testing.T, root string, pid int, start int64, executable string, argv ...string) string {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	comm := filepath.Base(executable)
	if len(comm) > 15 {
		comm = comm[:15]
	}
	stat := strings.Replace(gateProcessStat(pid, start), "(vpn-director-bo)", "("+comm+")", 1)
	for name, body := range map[string]string{
		"stat":    stat,
		"cmdline": strings.Join(argv, "\x00") + "\x00",
		"comm":    comm + "\n",
		"status":  "Name:\t" + comm + "\nUid:\t0\t0\t0\t0\nGid:\t0\t0\t0\t0\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(executable, filepath.Join(dir, "exe")); err != nil {
		t.Fatal(err)
	}
	return dir
}

// setProcessUID gives a fake process uid as its real, effective, saved and
// filesystem UID.
func setProcessUID(t *testing.T, dir, uid string) {
	t.Helper()
	status := fmt.Sprintf("Uid:\t%[1]s\t%[1]s\t%[1]s\t%[1]s\n", uid)
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0600); err != nil {
		t.Fatal(err)
	}
}

func checkGate(t *testing.T, g *Gate, compatible bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := g.Check(ctx)
	if compatible {
		if err != nil {
			t.Fatalf("gate closed: %v", err)
		}
	} else if !errors.Is(err, ErrIncompatible) {
		t.Fatalf("gate error %v, want ErrIncompatible", err)
	}
}

func TestGate_InstalledAndRunningExecutables(t *testing.T) {
	t.Run("missing bot and no process", func(t *testing.T) {
		checkGate(t, gateFixture(t), true)
	})
	t.Run("compatible stopped bot from root with exact flag", func(t *testing.T) {
		g := gateFixture(t)
		writeCapabilityExecutable(t, g.BotPath, capabilityReply(compatibleJSON))
		checkGate(t, g, true)
	})
	t.Run("strict installed response", func(t *testing.T) {
		for _, tc := range []struct {
			name, payload string
			compatible    bool
		}{
			{"whitespace", " \n" + compatibleJSON + "\t ", true},
			{"old protocol", `{"protocol_version":0,"watch_owner":"watchd"}`, false},
			{"future protocol", `{"protocol_version":2,"watch_owner":"watchd"}`, false},
			{"old owner", `{"protocol_version":1,"watch_owner":"bot"}`, false},
			{"missing owner", `{"protocol_version":1}`, false},
			{"missing version", `{"watch_owner":"watchd"}`, false},
			{"string version", `{"protocol_version":"1","watch_owner":"watchd"}`, false},
			{"null owner", `{"protocol_version":1,"watch_owner":null}`, false},
			{"unknown field", `{"protocol_version":1,"watch_owner":"watchd","extra":true}`, false},
			{"null", `null`, false},
			{"array", `[]`, false},
			{"empty output", "", false},
			{"truncated", `{"protocol_version":1`, false},
			{"second document", compatibleJSON + "\n{}", false},
			{"trailing text", compatibleJSON + " provider-payload", false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				g := gateFixture(t)
				writeCapabilityExecutable(t, g.BotPath, capabilityReply(tc.payload))
				checkGate(t, g, tc.compatible)
			})
		}
	})
	t.Run("old executable fails even with valid stdout", func(t *testing.T) {
		g := gateFixture(t)
		writeCapabilityExecutable(t, g.BotPath, capabilityReply(compatibleJSON)+"\nexit 23")
		checkGate(t, g, false)
	})
	t.Run("unreadable installed executable", func(t *testing.T) {
		g := gateFixture(t)
		writeCapabilityExecutable(t, g.BotPath, capabilityReply(compatibleJSON))
		if err := os.Chmod(g.BotPath, 0600); err != nil {
			t.Fatal(err)
		}
		checkGate(t, g, false)
	})
	t.Run("proc enumeration uncertainty", func(t *testing.T) {
		g := gateFixture(t)
		g.ProcRoot = filepath.Join(t.TempDir(), "missing-proc")
		checkGate(t, g, false)
	})
	t.Run("every running executable", func(t *testing.T) {
		g := gateFixture(t)
		writeCapabilityExecutable(t, g.BotPath, capabilityReply(compatibleJSON))
		other := filepath.Join(t.TempDir(), "vpn-director-bot")
		writeCapabilityExecutable(t, other, capabilityReply(compatibleJSON))
		writeBotProcess(t, g.ProcRoot, 41, 101, g.BotPath, g.BotPath)
		writeBotProcess(t, g.ProcRoot, 42, 102, other, g.BotPath, "--daemon")
		checkGate(t, g, true)
		old := filepath.Join(t.TempDir(), "vpn-director-bot")
		writeCapabilityExecutable(t, old, capabilityReply(`{"protocol_version":1,"watch_owner":"bot"}`))
		writeBotProcess(t, g.ProcRoot, 43, 103, old, g.BotPath)
		checkGate(t, g, false)
	})
	t.Run("missing installed file does not excuse an old process", func(t *testing.T) {
		g := gateFixture(t)
		old := filepath.Join(t.TempDir(), "vpn-director-bot")
		writeCapabilityExecutable(t, old, capabilityReply(`{}`))
		writeBotProcess(t, g.ProcRoot, 47, 104, old, g.BotPath)
		checkGate(t, g, false)
	})
	t.Run("running executable cannot be confirmed", func(t *testing.T) {
		for _, missing := range []string{"exe", "stat", "cmdline"} {
			t.Run(missing, func(t *testing.T) {
				g := gateFixture(t)
				writeCapabilityExecutable(t, g.BotPath, capabilityReply(compatibleJSON))
				dir := writeBotProcess(t, g.ProcRoot, 48, 105, g.BotPath, g.BotPath)
				if err := os.Remove(filepath.Join(dir, missing)); err != nil {
					t.Fatal(err)
				}
				checkGate(t, g, false)
			})
		}
	})
	t.Run("bot executable with unrelated argv zero", func(t *testing.T) {
		g := gateFixture(t)
		writeCapabilityExecutable(t, g.BotPath, capabilityReply(compatibleJSON))
		old := filepath.Join(t.TempDir(), "vpn-director-bot")
		writeCapabilityExecutable(t, old, capabilityReply(`{}`))
		writeBotProcess(t, g.ProcRoot, 49, 106, old, "custom-launch-name", "--daemon")
		checkGate(t, g, false)
	})
	t.Run("bot path only in later argv is not a bot", func(t *testing.T) {
		g := gateFixture(t)
		other := filepath.Join(t.TempDir(), "utility")
		writeCapabilityExecutable(t, other, "exit 93")
		writeBotProcess(t, g.ProcRoot, 50, 107, other, other, "--file", g.BotPath)
		checkGate(t, g, true)
	})
	t.Run("process that exited after the listing", func(t *testing.T) {
		g := gateFixture(t)
		writeCapabilityExecutable(t, g.BotPath, capabilityReply(compatibleJSON))
		writeBotProcess(t, g.ProcRoot, 52, 109, g.BotPath, g.BotPath)
		// Listed, while the PID directory and every file in it are gone.
		if err := os.Symlink(filepath.Join(t.TempDir(), "reaped"), filepath.Join(g.ProcRoot, "53")); err != nil {
			t.Fatal(err)
		}
		checkGate(t, g, true)
	})
	t.Run("unrelated process with a command line over 64 KiB", func(t *testing.T) {
		g := gateFixture(t)
		other := filepath.Join(t.TempDir(), "utility")
		writeCapabilityExecutable(t, other, "exit 93")
		writeBotProcess(t, g.ProcRoot, 54, 110, other, other, strings.Repeat("x", 64<<10))
		checkGate(t, g, true)
	})
	t.Run("replaced installed path leaves old proc exe", func(t *testing.T) {
		g := gateFixture(t)
		writeCapabilityExecutable(t, g.BotPath, capabilityReply(`{}`))
		old := filepath.Join(t.TempDir(), "vpn-director-bot")
		if err := os.Link(g.BotPath, old); err != nil {
			t.Fatal(err)
		}
		dir := writeBotProcess(t, g.ProcRoot, 51, 108, old, g.BotPath)
		replacement := filepath.Join(t.TempDir(), "vpn-director-bot")
		writeCapabilityExecutable(t, replacement, capabilityReply(compatibleJSON))
		if err := os.Rename(replacement, g.BotPath); err != nil {
			t.Fatal(err)
		}
		checkGate(t, g, false)
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		checkGate(t, g, true)
	})
	t.Run("two second subprocess deadline", func(t *testing.T) {
		g := gateFixture(t)
		writeCapabilityExecutable(t, g.BotPath, "exec /bin/sleep 10")
		start := time.Now()
		checkGate(t, g, false)
		if elapsed := time.Since(start); elapsed < 1500*time.Millisecond || elapsed > 3500*time.Millisecond {
			t.Fatalf("capability check took %v, want the 2s bound independent of the 5s caller", elapsed)
		}
	})
	t.Run("exact 4096 byte stdout cap includes whitespace", func(t *testing.T) {
		for _, size := range []int{4096, 4097} {
			t.Run(strconv.Itoa(size), func(t *testing.T) {
				g := gateFixture(t)
				payload := compatibleJSON + strings.Repeat(" ", size-len(compatibleJSON)-1)
				writeCapabilityExecutable(t, g.BotPath, capabilityReply(payload))
				checkGate(t, g, size == 4096)
			})
		}
	})
}

// Only root runs the bot: another user's process named like it - by its
// executable or a spoofed argv0 - is neither executed nor able to close the gate.
func TestGate_OnlyRootProcessesAreRunningBots(t *testing.T) {
	for _, tc := range []struct {
		name     string
		exe      string // the process's executable, by base name
		argv0Bot bool   // argv0 names the installed bot
		uid      string
		executed bool
	}{
		{"another user's executable named like the bot", "vpn-director-bot", false, "1000", false},
		{"another user's process with a spoofed argv0", "utility", true, "1000", false},
		{"root's executable named like the bot", "vpn-director-bot", false, "0", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gateFixture(t)
			writeCapabilityExecutable(t, g.BotPath, capabilityReply(compatibleJSON))
			marker := filepath.Join(t.TempDir(), "executed")
			running := filepath.Join(t.TempDir(), tc.exe)
			writeCapabilityExecutable(t, running, ": > "+shellQuote(marker)+"\n"+capabilityReply(`{"protocol_version":1,"watch_owner":"bot"}`))
			argv0 := running
			if tc.argv0Bot {
				argv0 = g.BotPath
			}
			setProcessUID(t, writeBotProcess(t, g.ProcRoot, 81, 401, running, argv0, "--daemon"), tc.uid)
			checkGate(t, g, !tc.executed)
			if _, err := os.Stat(marker); (err == nil) != tc.executed {
				t.Fatalf("executed %v (%v), want %v", err == nil, err, tc.executed)
			}
		})
	}
	t.Run("named like the bot with no command line", func(t *testing.T) {
		for _, tc := range []struct {
			uid        string
			compatible bool
		}{{"1000", true}, {"0", false}} {
			t.Run("uid "+tc.uid, func(t *testing.T) {
				g := gateFixture(t)
				writeCapabilityExecutable(t, g.BotPath, capabilityReply(compatibleJSON))
				dir := writeBotProcess(t, g.ProcRoot, 82, 402, g.BotPath)
				if err := os.WriteFile(filepath.Join(dir, "cmdline"), nil, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Join(dir, "exe")); err != nil {
					t.Fatal(err)
				}
				setProcessUID(t, dir, tc.uid)
				checkGate(t, g, tc.compatible)
			})
		}
	})
	t.Run("bot process whose owner cannot be read", func(t *testing.T) {
		for _, status := range []string{"missing", "no Uid line"} {
			t.Run(status, func(t *testing.T) {
				g := gateFixture(t)
				writeCapabilityExecutable(t, g.BotPath, capabilityReply(compatibleJSON))
				dir := writeBotProcess(t, g.ProcRoot, 83, 403, g.BotPath, g.BotPath)
				path := filepath.Join(dir, "status")
				var err error
				if status == "missing" {
					err = os.Remove(path)
				} else {
					err = os.WriteFile(path, []byte("Name:\tvpn-director-bo\nGid:\t0\t0\t0\t0\n"), 0600)
				}
				if err != nil {
					t.Fatal(err)
				}
				checkGate(t, g, false)
			})
		}
	})
}

func TestGate_DiagnosticsDoNotExposeExecutableOutput(t *testing.T) {
	g := gateFixture(t)
	payload := "provider-payload synthetic-credential https://provider.example/sub/synthetic-secret"
	writeCapabilityExecutable(t, g.BotPath, capabilityReply(payload)+"\n"+capabilityReply(payload)+" >&2\nexit 23")
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	err := g.Check(context.Background())
	if !errors.Is(err, ErrIncompatible) {
		t.Fatalf("gate error %v, want ErrIncompatible", err)
	}
	for _, secret := range []string{"provider-payload", "synthetic-credential", "https://provider.example/sub/synthetic-secret"} {
		if strings.Contains(err.Error(), secret) || strings.Contains(logs.String(), secret) {
			t.Errorf("capability diagnostics expose synthetic provider data: %q", secret)
		}
	}
}

// gateWarnings records the log for the rest of the test and returns, when
// called, every Warn record in it.
func gateWarnings(t *testing.T) func() []map[string]any {
	t.Helper()
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return func() []map[string]any {
		t.Helper()
		var warnings []map[string]any
		decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
		for {
			var record map[string]any
			if err := decoder.Decode(&record); errors.Is(err, io.EOF) {
				return warnings
			} else if err != nil {
				t.Fatal(err)
			}
			if record["level"] == "WARN" {
				warnings = append(warnings, record)
			}
		}
	}
}

// A root process named like the bot counts wherever it runs, so one whose
// reply keeps the gate closed is named in the log - once while it does.
func TestGate_RefusalNamesTheExecutable(t *testing.T) {
	t.Run("once per refusing target", func(t *testing.T) {
		g := gateFixture(t)
		warnings := gateWarnings(t)
		writeCapabilityExecutable(t, g.BotPath, capabilityReply(compatibleJSON))
		answer := filepath.Join(t.TempDir(), "answer")
		if err := os.WriteFile(answer, []byte(`{"protocol_version":1,"watch_owner":"bot"}`), 0600); err != nil {
			t.Fatal(err)
		}
		running := filepath.Join(t.TempDir(), "vpn-director-bot")
		writeCapabilityExecutable(t, running, fmt.Sprintf("exec /bin/cat %q", answer))
		writeBotProcess(t, g.ProcRoot, 91, 501, running, g.BotPath)
		assertWarned := func(want int) {
			t.Helper()
			got := warnings()
			if len(got) != want {
				t.Fatalf("Warn records %d, want %d: %v", len(got), want, got)
			}
			if last := got[len(got)-1]; last["pid"] != float64(91) || last["path"] != running {
				t.Fatalf("refusal names pid %v and path %v, want 91 and %s", last["pid"], last["path"], running)
			}
		}

		checkGate(t, g, false)
		assertWarned(1)
		checkGate(t, g, false)
		assertWarned(1)

		if err := os.WriteFile(answer, []byte(compatibleJSON), 0600); err != nil {
			t.Fatal(err)
		}
		checkGate(t, g, true)
		// A verified executable is not run again; the process running a
		// rebuilt one is, and its refusal is named again.
		writeCapabilityExecutable(t, running, capabilityReply(`{"protocol_version":1,"watch_owner":"bot"}`))
		checkGate(t, g, false)
		assertWarned(2)
	})
	t.Run("cancelled check", func(t *testing.T) {
		g := gateFixture(t)
		warnings := gateWarnings(t)
		writeCapabilityExecutable(t, g.BotPath, capabilityReply(compatibleJSON))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		g.exec = func(ctx context.Context, path string) ([]byte, error) {
			cancel()
			return nil, ctx.Err()
		}
		if err := g.Check(ctx); !errors.Is(err, ErrIncompatible) {
			t.Fatalf("cancelled check %v, want ErrIncompatible", err)
		}
		if got := warnings(); len(got) != 0 {
			t.Fatalf("Warn records %v; a cancelled check is no refusal", got)
		}
	})
}

func TestGate_CacheInvalidation(t *testing.T) {
	for _, change := range []string{"inode", "size", "mtime"} {
		t.Run(change, func(t *testing.T) {
			g := gateFixture(t)
			writeCapabilityExecutable(t, g.BotPath, capabilityReply(compatibleJSON))
			before, err := os.Stat(g.BotPath)
			if err != nil {
				t.Fatal(err)
			}
			checkGate(t, g, true)
			payload := `{"protocol_version":1,"watch_owner":"botbot"}`
			if change == "size" {
				payload = `{}`
			}
			path := g.BotPath
			if change == "inode" {
				path = filepath.Join(t.TempDir(), "vpn-director-bot")
			}
			writeCapabilityExecutable(t, path, capabilityReply(payload))
			mtime := before.ModTime()
			if change == "mtime" {
				mtime = mtime.Add(time.Second)
			}
			if err := os.Chtimes(path, mtime, mtime); err != nil {
				t.Fatal(err)
			}
			if change == "inode" {
				if err := os.Rename(path, g.BotPath); err != nil {
					t.Fatal(err)
				}
			}
			after, err := os.Stat(g.BotPath)
			if err != nil {
				t.Fatal(err)
			}
			if change != "size" && after.Size() != before.Size() {
				t.Fatal("fixture changed size instead of only the requested identity field")
			}
			if change != "mtime" && !after.ModTime().Equal(before.ModTime()) {
				t.Fatal("fixture changed mtime instead of only the requested identity field")
			}
			checkGate(t, g, false)
		})
	}
	// A reused PID that runs the verified executable runs the verified bytes and
	// is not run again; one that runs another executable is.
	t.Run("PID reuse with the same executable", func(t *testing.T) {
		g := gateFixture(t)
		writeCapabilityExecutable(t, g.BotPath, capabilityReply(compatibleJSON))
		runs := 0
		g.exec = func(ctx context.Context, path string) ([]byte, error) {
			runs++
			return readCapabilities(ctx, path)
		}
		dir := writeBotProcess(t, g.ProcRoot, 61, 201, g.BotPath, g.BotPath)
		checkGate(t, g, true)
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(gateProcessStat(61, 202)), 0600); err != nil {
			t.Fatal(err)
		}
		checkGate(t, g, true)
		if runs != 1 {
			t.Fatalf("capability runs %d, want 1: one executable is verified once", runs)
		}
	})
	t.Run("PID reuse with another executable", func(t *testing.T) {
		g := gateFixture(t)
		writeCapabilityExecutable(t, g.BotPath, capabilityReply(compatibleJSON))
		dir := writeBotProcess(t, g.ProcRoot, 61, 201, g.BotPath, g.BotPath)
		checkGate(t, g, true)
		old := filepath.Join(t.TempDir(), "vpn-director-bot")
		writeCapabilityExecutable(t, old, capabilityReply(`{}`))
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(gateProcessStat(61, 202)), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(dir, "exe")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(old, filepath.Join(dir, "exe")); err != nil {
			t.Fatal(err)
		}
		checkGate(t, g, false)
	})
}

func TestGate_CancellationKeepsTheVerifiedCache(t *testing.T) {
	for _, when := range []string{"before the check", "during the scan"} {
		t.Run(when, func(t *testing.T) {
			g := gateFixture(t)
			writeCapabilityExecutable(t, g.BotPath, capabilityReply(compatibleJSON))
			runs := 0
			g.exec = func(ctx context.Context, path string) ([]byte, error) {
				runs++
				return readCapabilities(ctx, path)
			}
			checkGate(t, g, true)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if when == "before the check" {
				cancel()
			} else {
				g.proc = func(ctx context.Context, root, botPath string) ([]executableTarget, error) {
					cancel()
					return runningBots(ctx, root, botPath)
				}
			}
			if err := g.Check(ctx); !errors.Is(err, ErrIncompatible) {
				t.Fatalf("cancelled check %v, want ErrIncompatible", err)
			}
			g.proc = nil
			checkGate(t, g, true)
			if runs != 1 {
				t.Fatalf("capability runs %d, want 1: a cancelled check is no evidence against the verified bot", runs)
			}
		})
	}
}

// A PID reused during the check by the executable just verified adds no
// unverified code; one reused by another executable does.
func TestGate_ProcessChangesDuringCheck(t *testing.T) {
	for _, tc := range []struct {
		change     string
		compatible bool
	}{{"PID reused by the same executable", true}, {"PID reused by another executable", false}, {"process vanished", true}} {
		t.Run(tc.change, func(t *testing.T) {
			g := gateFixture(t)
			writeCapabilityExecutable(t, g.BotPath, capabilityReply(compatibleJSON))
			running := filepath.Join(t.TempDir(), "vpn-director-bot")
			dir := filepath.Join(g.ProcRoot, "71")
			body := fmt.Sprintf("printf '%%s' %s > %q", shellQuote(gateProcessStat(71, 302)), filepath.Join(dir, "stat"))
			switch tc.change {
			case "PID reused by another executable":
				other := filepath.Join(t.TempDir(), "vpn-director-bot")
				writeCapabilityExecutable(t, other, capabilityReply(compatibleJSON))
				body += fmt.Sprintf("\n/bin/ln -sfn %q %q", other, filepath.Join(dir, "exe"))
			case "process vanished":
				body = fmt.Sprintf("/bin/rm -r -- %q", dir)
			}
			writeCapabilityExecutable(t, running, body+"\n"+capabilityReply(compatibleJSON))
			writeBotProcess(t, g.ProcRoot, 71, 301, running, g.BotPath)
			checkGate(t, g, tc.compatible)
			checkGate(t, g, true)
		})
	}
}

// A bot process that exits between the two scans adds no unverified code, so
// the check stands; one that appears between them was never run and refuses it.
func TestGate_ProcessesBetweenTheScans(t *testing.T) {
	for _, tc := range []struct {
		name          string
		first, second bool // the bot process is in the first, the second scan
		compatible    bool
	}{
		{"exited after the first scan", true, false, true},
		{"started after the first scan", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gateFixture(t)
			writeCapabilityExecutable(t, g.BotPath, capabilityReply(compatibleJSON))
			running := filepath.Join(t.TempDir(), "vpn-director-bot")
			writeCapabilityExecutable(t, running, capabilityReply(compatibleJSON))
			identity, err := identifyExecutable(running)
			if err != nil {
				t.Fatal(err)
			}
			bot := executableTarget{path: running, identity: identity, pid: 91, start: 501}
			scans, runs := 0, 0
			g.proc = func(context.Context, string, string) ([]executableTarget, error) {
				scans++
				if (scans == 1 && tc.first) || (scans == 2 && tc.second) {
					return []executableTarget{bot}, nil
				}
				return nil, nil
			}
			g.exec = func(ctx context.Context, path string) ([]byte, error) {
				runs++
				return readCapabilities(ctx, path)
			}
			checkGate(t, g, tc.compatible)
			if scans != 2 {
				t.Fatalf("scans %d, want 2", scans)
			}
			if !tc.compatible {
				return
			}
			if _, ok := g.cache[bot.identity]; ok {
				t.Fatal("the cache keeps an executable the second scan no longer saw")
			}
			// The verified set is what the second scan saw: the next check finds
			// it unchanged and runs nothing again.
			checkGate(t, g, true)
			if runs != 2 {
				t.Fatalf("capability runs %d, want 2", runs)
			}
		})
	}
}

// The bot's own child between fork and exec runs the bot's executable under a
// new PID. It adds no unverified code, so the check stands without another
// run, whichever scans see it.
func TestGate_AChildOfAVerifiedBotIsVerified(t *testing.T) {
	for _, tc := range []struct {
		name          string
		first, second bool // the child is in the first, the second scan
	}{
		{"in the second scan only", false, true},
		{"in both scans", true, true},
		{"in the first scan only", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gateFixture(t)
			writeCapabilityExecutable(t, g.BotPath, capabilityReply(compatibleJSON))
			identity, err := identifyExecutable(g.BotPath)
			if err != nil {
				t.Fatal(err)
			}
			bot := executableTarget{path: g.BotPath, identity: identity, pid: 90, start: 500}
			child := executableTarget{path: g.BotPath, identity: identity, pid: 93, start: 503}
			scans, runs := 0, 0
			g.proc = func(context.Context, string, string) ([]executableTarget, error) {
				scans++
				if (scans == 1 && tc.first) || (scans == 2 && tc.second) {
					return []executableTarget{bot, child}, nil
				}
				return []executableTarget{bot}, nil
			}
			g.exec = func(ctx context.Context, path string) ([]byte, error) {
				runs++
				return readCapabilities(ctx, path)
			}
			checkGate(t, g, true)
			if runs != 1 {
				t.Fatalf("capability runs %d, want 1: one executable, verified once", runs)
			}
		})
	}
}

// replaceExe points a fake process's exe link at executable.
func replaceExe(t *testing.T, dir, executable string) {
	t.Helper()
	if err := os.Remove(filepath.Join(dir, "exe")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(executable, filepath.Join(dir, "exe")); err != nil {
		t.Fatal(err)
	}
}

// A process whose first reads named the bot but that executes another program
// before the scan is done with it - the bot's child between fork and exec - no
// longer runs the bot: it is skipped, neither run nor a refusal.
func TestGate_AProcessThatExecsAwayIsSkipped(t *testing.T) {
	g := gateFixture(t)
	writeCapabilityExecutable(t, g.BotPath, capabilityReply(compatibleJSON))
	utility := filepath.Join(t.TempDir(), "sh")
	writeCapabilityExecutable(t, utility, "exit 93")
	dir := writeBotProcess(t, g.ProcRoot, 96, 601, g.BotPath, g.BotPath)
	previous := readProcExe
	t.Cleanup(func() { readProcExe = previous })
	readProcExe = func(d string) (string, error) {
		link, err := previous(d)
		if d == dir && err == nil && link == g.BotPath {
			// The exec lands right after this read.
			replaceExe(t, dir, utility)
			if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(utility+"\x00"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		return link, err
	}
	checkGate(t, g, true)
}

// A bot process that exits between the two reads of its stat runs nothing: it
// is skipped, not a refusal.
func TestGate_AProcessThatExitsBetweenTheStatReadsIsSkipped(t *testing.T) {
	g := gateFixture(t)
	writeCapabilityExecutable(t, g.BotPath, capabilityReply(compatibleJSON))
	dir := writeBotProcess(t, g.ProcRoot, 97, 701, g.BotPath, g.BotPath)
	previous := readProcStat
	t.Cleanup(func() { readProcStat = previous })
	reads := 0
	readProcStat = func(d string) ([]byte, error) {
		body, err := previous(d)
		if d == dir {
			reads++
			if reads == 2 {
				return []byte(strings.Replace(gateProcessStat(97, 701), ") S ", ") Z ", 1)), nil
			}
		}
		return body, err
	}
	checkGate(t, g, true)
	if reads < 2 {
		t.Fatalf("stat reads %d, want the scan to read it twice", reads)
	}
}

// A refusal that runs no executable - a scan that cannot vouch for a process, a
// bot that starts during the check - is named in the log too, once for as long
// as it keeps the gate closed.
func TestGate_RefusalsWithoutARunAreLogged(t *testing.T) {
	g := gateFixture(t)
	warnings := gateWarnings(t)
	writeCapabilityExecutable(t, g.BotPath, capabilityReply(compatibleJSON))
	other := filepath.Join(t.TempDir(), "vpn-director-bot")
	writeCapabilityExecutable(t, other, capabilityReply(compatibleJSON))
	identity, err := identifyExecutable(other)
	if err != nil {
		t.Fatal(err)
	}
	late := executableTarget{path: other, identity: identity, pid: 98, start: 801}
	scans := 0
	g.proc = func(context.Context, string, string) ([]executableTarget, error) {
		scans++
		if scans%2 == 0 {
			return []executableTarget{late}, nil
		}
		return nil, nil
	}
	checkGate(t, g, false)
	checkGate(t, g, false)
	got := warnings()
	if len(got) != 1 || got[0]["pid"] != float64(98) || got[0]["path"] != other {
		t.Fatalf("Warn records %v, want one naming pid 98 and %s", got, other)
	}

	g.proc = func(context.Context, string, string) ([]executableTarget, error) {
		return nil, fmt.Errorf("%w: pid 99: owner unreadable", ErrIncompatible)
	}
	checkGate(t, g, false)
	checkGate(t, g, false)
	got = warnings()
	if len(got) != 2 || !strings.Contains(fmt.Sprint(got[1]["reason"]), "pid 99: owner unreadable") {
		t.Fatalf("Warn records %v, want a second one giving the scan's reason", got)
	}

	g.proc = nil
	checkGate(t, g, true)
	g.proc = func(context.Context, string, string) ([]executableTarget, error) {
		return nil, fmt.Errorf("%w: pid 99: owner unreadable", ErrIncompatible)
	}
	checkGate(t, g, false)
	if got = warnings(); len(got) != 3 {
		t.Fatalf("Warn records %d, want the refusal named again after a check passed", len(got))
	}
}
