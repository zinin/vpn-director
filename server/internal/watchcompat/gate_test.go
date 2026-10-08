//go:build linux

package watchcompat

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	t.Run("PID reuse with same executable fingerprint", func(t *testing.T) {
		g := gateFixture(t)
		writeCapabilityExecutable(t, g.BotPath, capabilityReply(compatibleJSON))
		answer := filepath.Join(t.TempDir(), "answer")
		if err := os.WriteFile(answer, []byte(compatibleJSON), 0600); err != nil {
			t.Fatal(err)
		}
		running := filepath.Join(t.TempDir(), "vpn-director-bot")
		writeCapabilityExecutable(t, running, fmt.Sprintf("exec /bin/cat %q", answer))
		dir := writeBotProcess(t, g.ProcRoot, 61, 201, running, g.BotPath)
		checkGate(t, g, true)
		if err := os.WriteFile(answer, []byte(`{}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(gateProcessStat(61, 202)), 0600); err != nil {
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

func TestGate_ProcessChangesDuringCheck(t *testing.T) {
	for _, change := range []string{"PID reused", "process vanished"} {
		t.Run(change, func(t *testing.T) {
			g := gateFixture(t)
			writeCapabilityExecutable(t, g.BotPath, capabilityReply(compatibleJSON))
			running := filepath.Join(t.TempDir(), "vpn-director-bot")
			dir := filepath.Join(g.ProcRoot, "71")
			body := fmt.Sprintf("printf '%%s' %s > %q", shellQuote(gateProcessStat(71, 302)), filepath.Join(dir, "stat"))
			if change == "process vanished" {
				body = fmt.Sprintf("/bin/rm -r -- %q", dir)
			}
			writeCapabilityExecutable(t, running, body+"\n"+capabilityReply(compatibleJSON))
			writeBotProcess(t, g.ProcRoot, 71, 301, running, g.BotPath)
			checkGate(t, g, false)
			checkGate(t, g, true)
		})
	}
}
