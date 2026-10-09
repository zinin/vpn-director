package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCapabilities_EarlyReadOnlyCLI(t *testing.T) {
	if raw := os.Getenv("VPD_TASK8_BOT_MAIN"); raw != "" {
		var args []string
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			panic(err)
		}
		os.Args = append([]string{"telegram-bot"}, args...)
		flag.CommandLine = flag.NewFlagSet("telegram-bot", flag.ExitOnError)
		main()
		os.Exit(0)
	}

	for _, tc := range []struct {
		name string
		args []string
		caps bool
	}{
		{"no_token_or_detectable_platform", []string{"--watchd-capabilities"}, true},
		{"invalid_platform_is_not_resolved", []string{"--watchd-capabilities", "--platform", "invalid-platform"}, true},
		{"dev_files_are_not_read", []string{"--watchd-capabilities", "--dev"}, true},
		{"self_update_first_argument_wins", []string{"self-update", "--watchd-capabilities"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			dev := filepath.Join(dir, "testdata", "dev")
			if err := os.MkdirAll(bin, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(dev, 0700); err != nil {
				t.Fatal(err)
			}
			invalidConfig := filepath.Join(dev, "telegram-bot.json")
			if err := os.WriteFile(invalidConfig, []byte("not a bot config"), 0600); err != nil {
				t.Fatal(err)
			}
			callsPath := filepath.Join(dir, "calls")
			for _, name := range []string{"nvram", "ndmc", "vpn-director.sh", "curl", "wget", "xray"} {
				if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nprintf 'called\\n' >> \"$VPD_TASK8_CALLS\"\nexit 99\n"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			args, err := json.Marshal(tc.args)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCapabilities_EarlyReadOnlyCLI$")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "VPD_TASK8_BOT_MAIN="+string(args), "VPD_TASK8_CALLS="+callsPath,
				"PATH="+bin, "VPD_PLATFORM=invalid-platform", "VPD_PROBE_ROOT="+dir)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err = cmd.Run()
			if ctx.Err() != nil {
				t.Fatal("read-only capability CLI exceeded 2 seconds")
			}
			if tc.caps {
				if err != nil || stdout.String() != "{\"protocol_version\":1,\"watch_owner\":\"watchd\"}\n" || stderr.Len() != 0 {
					t.Errorf("early capabilities: err=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
				}
			} else if err == nil || stdout.Len() != 0 || !strings.Contains(stderr.String(), "flag provided but not defined: -watchd-capabilities") {
				t.Errorf("self-update did not retain priority: err=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
			}
			calls := 0
			if raw, err := os.ReadFile(callsPath); err == nil {
				calls = bytes.Count(raw, []byte("called\n"))
			} else if !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if calls != 0 {
				t.Errorf("capability/self-update startup invoked platform/network/shell services: calls=%d, want 0", calls)
			}
			if raw, err := os.ReadFile(invalidConfig); err != nil || string(raw) != "not a bot config" {
				t.Fatal("read-only invocation changed bot config")
			}
			for _, path := range []string{"bot.log", "xray.json", "data/chats.json", "data/watchd-notifications.json"} {
				if _, err := os.Stat(filepath.Join(dev, path)); !os.IsNotExist(err) {
					t.Errorf("early CLI created %s before dependencies: %v", path, err)
				}
			}
		})
	}
}

// runMainEnv makes the test binary run main() instead of its tests, so a test
// can start the real entry point as a child process.
const runMainEnv = "VPD_TEST_RUN_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(runMainEnv) == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// TestSelfUpdateIsDispatchedBeforeTheDaemonStarts pins the entry point of the
// self-update contract (internal/updater/selfupdate.go): the version being
// replaced runs this binary as "self-update ...", and nothing a daemon does at
// startup may come first. This binary is a dev build, so step 2 refuses the
// version it is asked to install - and only step 2 prints that refusal.
func TestSelfUpdateIsDispatchedBeforeTheDaemonStarts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0],
		"self-update", "--from", "v9.9.8", "--to", "v1.0.0", "--initiator", "bot", "--chat-id", "0")
	cmd.Env = append(os.Environ(), runMainEnv+"=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	err := cmd.Run()

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("exit = %v, want status 1\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "asked to install v1.0.0") {
		t.Errorf("stderr = %q, want step 2's version refusal", stderr.String())
	}
	// Contract item 3: every stdout line is a progress line for the user, and
	// this daemon's logger writes to stdout - a dispatch that ever moved below
	// a startup log line would put that line in front of the user.
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing but what step 2 prints", stdout.String())
	}
}
