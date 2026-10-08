//go:build linux

package updater

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/monitor"
	"github.com/zinin/vpn-director/server/internal/notifications"
	"github.com/zinin/vpn-director/server/internal/service"
	"github.com/zinin/vpn-director/server/internal/subwatch"
	"github.com/zinin/vpn-director/server/internal/watchcompat"
	"github.com/zinin/vpn-director/server/internal/watchdapi"
)

const migrationRestore = `{"data_dir":"data","xray":{"clients":["192.168.50.8"],"active_server":{"name":"Migration","address":"migration.example","port":443,"subscription":"0a1b2c3d","seq":7},"pending_restore":{"snapshot":{"tunnel":"wgc1","clients":["192.168.50.8"],"added":["192.168.50.8"],"committed":true},"restored":["192.168.50.8"],"active":{"name":"Migration","address":"migration.example","port":443,"subscription":"0a1b2c3d","seq":7}}}}`

type migrationObservation struct {
	Watch         watchdapi.WatchState
	Monitor       watchdapi.State
	Applies       int
	ConfigChanged bool
	Pending       bool
	Messages      int
}

type migrationSource struct {
	*notifications.Store
	watch *subwatch.Watch
}

func (s migrationSource) WatchSnapshot() watchdapi.WatchSnapshot {
	snapshot := s.watch.Snapshot()
	snapshot.Notifications = s.Status()
	return snapshot
}

// The init fixture invokes the already-built test executable, not a new build.
func TestWatchMigration_RuntimeChild(t *testing.T) {
	if os.Getenv("VPD_MIGRATION_CHILD") != "1" {
		t.Skip("only invoked by the owned watchd init fixture")
	}
	root := os.Getenv("VPD_MIGRATION_ROOT")
	gate := &watchcompat.Gate{BotPath: filepath.Join(root, "opt/vpn-director/telegram-bot"), ProcRoot: filepath.Join(root, "state/proc")}
	observation := observeMigrationRuntime(t, gate)
	body, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Join(root, "state/watch-starts.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(append(body, '\n')); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func observeMigrationRuntime(t *testing.T, gate *watchcompat.Gate) migrationObservation {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	cfg := service.NewConfigService(t.TempDir(), "")
	if err := os.WriteFile(cfg.ConfigPath(), []byte(migrationRestore), 0600); err != nil {
		t.Fatal(err)
	}
	queue, err := notifications.NewStore(filepath.Join(t.TempDir(), "watchd-notifications.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.ReplaceRecipients([]watchdapi.Recipient{{ChatID: 100, FirstSeen: time.Now().Add(-time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	applies := 0
	watch := &subwatch.Watch{
		LoadVPN: cfg.LoadVPNConfig, UpdateVPN: cfg.UpdateVPNConfig,
		CanMutate:   func() error { return gate.Check(ctx) },
		TPROXYReady: func() bool { return true },
		Apply:       func() error { applies++; return nil },
		Notify: func(text string) {
			if _, err := queue.Publish(text); err != nil {
				t.Error(err)
			}
		},
	}
	watch.Tick(ctx)
	watch.Tick(ctx)
	m := monitor.New(monitor.Deps{
		Settings: func() (monitor.Settings, error) { settings, _ := monitor.SettingsFrom(nil); return settings, nil },
		Endpoints: func() ([]monitor.Endpoint, map[string]string, error) {
			return []monitor.Endpoint{{Key: "ff-migration", Outbound: json.RawMessage(`{"protocol":"freedom","settings":{}}`)}}, nil, nil
		},
		Launcher: monitor.FakeLauncher{}, Stopped: func() bool { return false },
		WANUp: func(context.Context) bool { return true },
	})
	socketDir, err := os.MkdirTemp("", "vpd-migration-socket-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(socketDir)
	socket := filepath.Join(socketDir, "watchd.sock")
	listener, err := watchdapi.Listen(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	first, err := os.Lstat(socket)
	if err != nil || first.Mode().Perm() != 0600 {
		t.Fatalf("migration socket is not root-only: %v", err)
	}
	monitorDone := make(chan struct{})
	go func() { defer close(monitorDone); m.Run(ctx) }()
	serveDone := make(chan error, 1)
	go func() { serveDone <- watchdapi.ServeListener(ctx, listener, m, migrationSource{queue, watch}) }()
	defer func() {
		cancel()
		select {
		case <-monitorDone:
		case <-time.After(time.Second):
			t.Error("migration monitor did not drain")
		}
		select {
		case err := <-serveDone:
			if err != nil {
				t.Error("migration socket shutdown:", err)
			}
		case <-time.After(time.Second):
			t.Error("migration API did not drain")
		}
	}()
	client := watchdapi.NewClient(socket)
	var snapshot watchdapi.Snapshot
	for {
		snapshot, err = client.Monitor(ctx)
		if err != nil {
			t.Fatal("monitor API unavailable during bot migration:", err)
		}
		if snapshot.Endpoints["ff-migration"].Status == watchdapi.StatusAlive {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("bot compatibility prevented the independent monitor from probing")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if snapshot.State != watchdapi.StateOK {
		t.Fatalf("migration monitor state = %s, want ok", snapshot.State)
	}
	duplicate, err := watchdapi.Listen(ctx, socket)
	if err == nil {
		duplicate.Close()
		t.Fatal("second migration runtime replaced the first owner")
	}
	if current, err := os.Lstat(socket); err != nil || !os.SameFile(first, current) {
		t.Fatal("second migration runtime changed the first socket")
	}
	state, err := client.Watch(ctx)
	if err != nil {
		t.Fatal("watch API unavailable during compatibility waiting:", err)
	}
	page, err := client.Pending(ctx, "")
	if err != nil {
		t.Fatal("queue API unavailable during compatibility waiting:", err)
	}
	after, err := os.ReadFile(cfg.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	current, err := cfg.LoadVPNConfig()
	if err != nil {
		t.Fatal(err)
	}
	return migrationObservation{
		Watch: state.State, Monitor: snapshot.State, Applies: applies,
		ConfigChanged: !bytes.Equal(after, []byte(migrationRestore)),
		Pending:       current.Xray.PendingRestore != nil, Messages: len(page.Messages),
	}
}

func assertMigrationObservation(t *testing.T, got migrationObservation, want watchdapi.WatchState) {
	t.Helper()
	if got.Watch != want || got.Monitor != watchdapi.StateOK {
		t.Fatalf("migration status = %+v, want watch=%s and independent monitor=ok", got, want)
	}
	switch want {
	case watchdapi.WatchIncompatible:
		if got.Applies != 0 || got.ConfigChanged || !got.Pending || got.Messages != 0 {
			t.Fatalf("old bot allowed a second automation owner or lost pending intent: %+v", got)
		}
	case watchdapi.WatchActive:
		if got.Applies != 1 || !got.ConfigChanged || got.Pending || got.Messages != 1 {
			t.Fatalf("compatible migration did not finish restore exactly once: %+v", got)
		}
	default:
		t.Fatalf("unhandled migration expectation %s", want)
	}
}

type migrationSandbox struct {
	*firstInstallSandbox
	gate *watchcompat.Gate
}

func newMigrationSandbox(t *testing.T, botMode string, opts firstInstallOptions) *migrationSandbox {
	t.Helper()
	s := newFirstInstallSandbox(t, opts)
	proc := filepath.Join(s.state, "proc")
	if err := os.MkdirAll(proc, 0700); err != nil {
		t.Fatal(err)
	}
	capabilities := func(owner string) string {
		return "#!/bin/sh\n[ \"$#\" = 1 ] && [ \"$1\" = --watchd-capabilities ] || exit 91\n" +
			"[ \"$(pwd -P)\" = / ] || exit 92\nprintf '%s\\n' '{\"protocol_version\":1,\"watch_owner\":\"" + owner + "\"}'\n"
	}
	compatible := capabilities("watchd")
	s.binaryPayload = map[string]string{DaemonBot: compatible}
	s.write(s.binary(DaemonBot), capabilities("bot"), 0755)
	if botMode == "compatible-stopped" || botMode == "no-token" || botMode == "compatible-running" {
		s.write(s.binary(DaemonBot), compatible, 0755)
	}
	if botMode == "old-stopped" || botMode == "compatible-stopped" || botMode == "no-token" || botMode == "absent" {
		for _, suffix := range []string{".running", ".monitored"} {
			if err := os.Remove(filepath.Join(s.state, DaemonBot+suffix)); err != nil {
				t.Fatal(err)
			}
		}
		s.originalRunning = []string{DaemonWebUI}
		if opts.webuiStopped {
			s.originalRunning = nil
		}
	}
	if botMode == "absent" || botMode == "deleted-running" {
		if err := os.Remove(s.binary(DaemonBot)); err != nil {
			t.Fatal(err)
		}
		if botMode == "absent" {
			s.absentDaemons = append(s.absentDaemons, DaemonBot)
		}
	}
	if botMode == "no-token" {
		s.write(filepath.Join(s.root, "opt/vpn-director/telegram-bot.json"), `{"bot_token":"","allowed_users":[],"log_level":"info","update_check_interval":"1h"}`+"\n", 0600)
	}
	if opts.watchdExists && botMode == "old-running" {
		s.write(filepath.Join(s.state, DaemonWatchd+".running"), "", 0644)
		s.write(filepath.Join(s.state, DaemonWatchd+".monitored"), "", 0644)
		s.originalRunning = append(s.originalRunning, DaemonWatchd)
	}
	if s.contains(s.originalRunning, DaemonBot) {
		name := "telegram-bot"
		if botMode == "deleted-running" {
			name += " (deleted)"
		}
		old := filepath.Join(s.root, "old", name)
		body := capabilities("bot")
		if botMode == "compatible-running" {
			body = compatible
		}
		s.write(old, body, 0755)
		writeMigrationProcess(t, s, "41", old)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s.env = []string{"VPD_MIGRATION_CHILD=1", "VPD_MIGRATION_ROOT=" + s.root, "VPD_MIGRATION_TEST_EXE=" + executable}
	botInit := "#!/bin/sh\nname=telegram-bot\n" + sandboxInit
	botInit = strings.Replace(botInit, "        : > \"$STATE_DIR/$name.running\"", `        : > "$STATE_DIR/$name.running"
        /bin/mkdir -p "$STATE_DIR/proc/41"
        printf '41 (telegram-bot) S 1 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 202 0\n' > "$STATE_DIR/proc/41/stat"
        printf '%s\000' "$SANDBOX_ROOT/opt/vpn-director/telegram-bot" > "$STATE_DIR/proc/41/cmdline"
        printf 'Name:\ttelegram-bot\nUid:\t0\t0\t0\t0\nGid:\t0\t0\t0\t0\n' > "$STATE_DIR/proc/41/status"
        /bin/ln -sf "$SANDBOX_ROOT/opt/vpn-director/telegram-bot" "$STATE_DIR/proc/41/exe"`, 1)
	botInit = strings.Replace(botInit, `        /bin/rm -f "$STATE_DIR/$name.running"`, `        /bin/rm -f "$STATE_DIR/$name.running"
        /bin/rm -rf "$STATE_DIR/proc/41"`, 1)
	s.write(filepath.Join(s.root, "opt/etc/init.d/S98telegram-bot"), botInit, 0755)
	watchInit := "#!/bin/sh\nname=vpn-director-watchd\n" + sandboxInit
	watchInit = strings.Replace(watchInit, `        : > "$STATE_DIR/$name.running"`, `        : > "$STATE_DIR/$name.running"
        "$VPD_MIGRATION_TEST_EXE" -test.run '^TestWatchMigration_RuntimeChild$' -test.count=1 || exit 93`, 1)
	s.write(filepath.Join(s.root, "opt/etc/init.d/S98vpn-director-watchd"), watchInit, 0755)
	return &migrationSandbox{s, &watchcompat.Gate{BotPath: s.binary(DaemonBot), ProcRoot: proc}}
}

func writeMigrationProcess(t *testing.T, s *firstInstallSandbox, pid, executable string) {
	t.Helper()
	dir := filepath.Join(s.state, "proc", pid)
	s.write(filepath.Join(dir, "stat"), fmt.Sprintf("%s (telegram-bot) S 1 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 101 0\n", pid), 0600)
	s.write(filepath.Join(dir, "cmdline"), s.binary(DaemonBot)+"\x00", 0600)
	s.write(filepath.Join(dir, "status"), "Name:\ttelegram-bot\nUid:\t0\t0\t0\t0\nGid:\t0\t0\t0\t0\n", 0600)
	if err := os.Symlink(executable, filepath.Join(dir, "exe")); err != nil {
		t.Fatal(err)
	}
}

func (s *migrationSandbox) assertStarts(want watchdapi.WatchState, count int) {
	s.t.Helper()
	body, err := os.ReadFile(filepath.Join(s.state, "watch-starts.jsonl"))
	if os.IsNotExist(err) && count == 0 {
		return
	}
	if err != nil {
		s.t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != count {
		s.t.Fatalf("watch runtime starts = %d, want %d: %s", len(lines), count, body)
	}
	for _, line := range lines {
		var observation migrationObservation
		if err := json.Unmarshal([]byte(line), &observation); err != nil {
			s.t.Fatal(err)
		}
		assertMigrationObservation(s.t, observation, want)
	}
}

func TestWatchMigration_FullAndPartialUpdate(t *testing.T) {
	for _, tc := range []struct {
		name, bot, failure string
		options            firstInstallOptions
		startState         watchdapi.WatchState
		watchStarts        int
	}{
		{"first watchd and new bot", "old-running", "", firstInstallOptions{}, watchdapi.WatchActive, 1},
		{"installed old bot stays stopped after upgrade", "old-stopped", "", firstInstallOptions{}, watchdapi.WatchActive, 1},
		{"old deleted executable is stopped before compatible restart", "deleted-running", "", firstInstallOptions{}, watchdapi.WatchActive, 1},
		{"old bot copy failure retains incompatible running watchd", "old-running", "copy-old", firstInstallOptions{watchdExists: true}, watchdapi.WatchIncompatible, 1},
		{"old stopped bot copy failure leaves watchd stopped", "old-stopped", "copy-old", firstInstallOptions{watchdExists: true}, watchdapi.WatchIncompatible, 0},
		{"failure before the first file copy", "old-running", "before-copy", firstInstallOptions{}, watchdapi.WatchIncompatible, 0},
		{"first watchd partial copy", "old-running", "partial-copy", firstInstallOptions{}, watchdapi.WatchActive, 0},
		{"first watchd permissions", "old-running", "permissions", firstInstallOptions{}, watchdapi.WatchActive, 0},
		{"failure after both binary copies", "old-running", "after-copy", firstInstallOptions{}, watchdapi.WatchActive, 0},
		{"existing webui start failure does not introduce watchd", "old-running", "webui-start", firstInstallOptions{}, watchdapi.WatchActive, 0},
		{"new watchd start fails after runtime starts", "old-running", "vpn-director-watchd-start", firstInstallOptions{}, watchdapi.WatchActive, 1},
		{"bot start failure stops new watchd", "old-running", "telegram-bot-start", firstInstallOptions{}, watchdapi.WatchActive, 1},
		{"notify failure stops new watchd", "old-running", "notify", firstInstallOptions{}, watchdapi.WatchActive, 1},
		{"late failure stops remonitored new watchd", "old-running", "late-lock", firstInstallOptions{}, watchdapi.WatchActive, 1},
		{"stopped webui stays stopped during recovery", "old-running", "after-copy", firstInstallOptions{webuiStopped: true}, watchdapi.WatchActive, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newMigrationSandbox(t, tc.bot, tc.options)
			assertMigrationObservation(t, observeMigrationRuntime(t, s.gate), watchdapi.WatchIncompatible)
			result := s.run(tc.failure, "")
			if tc.failure != "" && !strings.Contains(result.calls, "FAIL "+tc.failure+"\n") {
				t.Fatal("migration did not reach the intended fault")
			}
			s.assertStarts(tc.startState, tc.watchStarts)
			assertMigrationObservation(t, observeMigrationRuntime(t, s.gate), tc.startState)
			for _, name := range []string{DaemonBot, DaemonWatchd, DaemonWebUI} {
				want := s.contains(s.originalRunning, name) || (tc.failure == "" && s.contains(s.newDaemons, name))
				s.assertState(name, want)
				if !want && !s.contains(s.newDaemons, name) && strings.Contains(result.calls, name+" start\n") {
					t.Errorf("migration started originally stopped %s", name)
				}
			}
			if tc.failure == "" {
				watchAt := strings.Index(result.calls, DaemonWatchd+" start\n")
				botAt := strings.LastIndex(result.calls, DaemonBot+" start\n")
				if tc.bot != "old-stopped" && (watchAt < 0 || botAt < watchAt) {
					t.Fatal("compatible bot must start after the new watchd runtime")
				}
			} else if !tc.options.watchdExists {
				if _, err := os.Lstat(s.binary(DaemonWatchd)); !os.IsNotExist(err) {
					t.Errorf("failed introduction left a first-copy binary behind: %v", err)
				}
			}
			s.assertIndependentFiles()
			if tc.failure != "" {
				if err := os.Remove(filepath.Join(s.state, "watch-starts.jsonl")); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
				retry := s.run("", "")
				starts := 1
				if tc.options.watchdExists && tc.bot == "old-stopped" {
					starts = 0
				}
				s.assertStarts(watchdapi.WatchActive, starts)
				assertMigrationObservation(t, observeMigrationRuntime(t, s.gate), watchdapi.WatchActive)
				for _, name := range []string{DaemonBot, DaemonWatchd, DaemonWebUI} {
					want := s.contains(s.originalRunning, name) || s.contains(s.newDaemons, name)
					s.assertState(name, want)
					if !want && strings.Contains(retry.calls, name+" start\n") {
						t.Errorf("successful retry started originally stopped %s", name)
					}
				}
				s.assertIndependentFiles()
			}
		})
	}
	// A replaced installed path cannot attest an old, deleted running executable.
	t.Run("retained deleted old executable blocks until it exits", func(t *testing.T) {
		s := newMigrationSandbox(t, "old-running", firstInstallOptions{})
		s.run("", "")
		assertMigrationObservation(t, observeMigrationRuntime(t, s.gate), watchdapi.WatchActive)
		old := filepath.Join(s.root, "old/telegram-bot (deleted)")
		s.write(old, "#!/bin/sh\nexit 64\n", 0755)
		writeMigrationProcess(t, s.firstInstallSandbox, "42", old)
		assertMigrationObservation(t, observeMigrationRuntime(t, s.gate), watchdapi.WatchIncompatible)
		if err := os.RemoveAll(filepath.Join(s.state, "proc/42")); err != nil {
			t.Fatal(err)
		}
		assertMigrationObservation(t, observeMigrationRuntime(t, s.gate), watchdapi.WatchActive)
		s.assertState(DaemonWatchd, true)
		s.assertIndependentFiles()
	})
}

func TestWatchMigration_MissingStoppedAndNoTokenBot(t *testing.T) {
	for _, bot := range []string{"absent", "compatible-stopped", "no-token"} {
		t.Run(bot, func(t *testing.T) {
			s := newMigrationSandbox(t, bot, firstInstallOptions{})
			assertMigrationObservation(t, observeMigrationRuntime(t, s.gate), watchdapi.WatchActive)
			result := s.run("", "")
			s.assertStarts(watchdapi.WatchActive, 1)
			assertMigrationObservation(t, observeMigrationRuntime(t, s.gate), watchdapi.WatchActive)
			s.assertState(DaemonWatchd, true)
			s.assertState(DaemonWebUI, true)
			s.assertState(DaemonBot, false)
			if strings.Contains(result.calls, DaemonBot+" start\n") || strings.Contains(result.calls, "monit monitor "+DaemonBot+"\n") {
				t.Fatal("absent/compatible stopped/no-token bot was started by migration")
			}
			if bot == "no-token" {
				data, err := os.ReadFile(filepath.Join(s.root, "opt/vpn-director/telegram-bot.json"))
				if err != nil || string(data) != `{"bot_token":"","allowed_users":[],"log_level":"info","update_check_interval":"1h"}`+"\n" {
					t.Fatal("migration changed the no-token bot configuration")
				}
			}
			s.assertIndependentFiles()
		})
	}
}
