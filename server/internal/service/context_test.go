package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/zinin/vpn-director/server/internal/shell"
)

type contextOperation struct {
	ctx  context.Context
	name string
	args []string
}

type contextBlockingExecutor struct {
	started chan contextOperation
}

func (e *contextBlockingExecutor) Exec(ctx context.Context, name string, args ...string) (*shell.Result, error) {
	e.started <- contextOperation{ctx: ctx, name: name, args: append([]string{}, args...)}
	<-ctx.Done()
	return &shell.Result{ExitCode: -1}, ctx.Err()
}

func TestContextExecutor_ShutdownCancelsOperations(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		timeout time.Duration
		run     func(*VPNDirectorService) error
	}{
		{"apply", []string{"--wait", "--unless-stopped", "apply"}, 5 * time.Minute, (*VPNDirectorService).ApplyUnlessStopped},
		{"restart_process", []string{"--wait", "--unless-stopped", "restart", "xray-process"}, 5 * time.Minute, (*VPNDirectorService).RestartXrayProcessUnlessStopped},
		{"platform", []string{"platform"}, 30 * time.Second, func(s *VPNDirectorService) error { _, err := s.Platform(); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, cancel := context.WithCancel(context.Background())
			defer cancel()
			executor := &contextBlockingExecutor{started: make(chan contextOperation, 1)}
			svc := NewVPNDirectorService("/synthetic/vpn-director", WithContext(root, executor))
			result := make(chan error, 1)
			go func() { result <- tc.run(svc) }()
			var op contextOperation
			select {
			case op = <-executor.started:
			case <-time.After(2 * time.Second):
				t.Fatal("scoped service did not start its operation")
			}
			if op.name != "/synthetic/vpn-director/vpn-director.sh" || !reflect.DeepEqual(op.args, tc.args) {
				t.Errorf("operation = %s %q, want the guarded command %q", op.name, op.args, tc.args)
			}
			if deadline, ok := op.ctx.Deadline(); !ok || time.Until(deadline) <= 0 || time.Until(deadline) > tc.timeout {
				t.Errorf("service lost its independent command timeout: deadline=%v present=%v", deadline, ok)
			}
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) || !errors.Is(op.ctx.Err(), context.Canceled) {
					t.Errorf("shutdown error=%v, executor context=%v; want root cancellation", err, op.ctx.Err())
				}
			case <-time.After(2 * time.Second):
				t.Fatal("root shutdown left a shell/platform operation running")
			}
		})
	}
}

func TestContextExecutor_CommandDeadlineStillWins(t *testing.T) {
	root, cancelRoot := context.WithCancel(context.Background())
	defer cancelRoot()
	command, cancelCommand := context.WithCancel(context.Background())
	defer cancelCommand()
	executor := &contextBlockingExecutor{started: make(chan contextOperation, 1)}
	result := make(chan error, 1)
	go func() { _, err := WithContext(root, executor).Exec(command, "synthetic"); result <- err }()
	select {
	case <-executor.started:
	case <-time.After(2 * time.Second):
		t.Fatal("executor did not start")
	}
	cancelCommand()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) || root.Err() != nil {
			t.Fatalf("command cancellation=%v root=%v", err, root.Err())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("root context replaced rather than combined with the command context")
	}

	deadline, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stop()
	if _, err := WithContext(root, executor).Exec(deadline, "synthetic"); !errors.Is(err, context.DeadlineExceeded) || root.Err() != nil {
		t.Fatalf("command deadline=%v root=%v", err, root.Err())
	}
}

func TestContextExecutor_RootDeadlineKeepsItsIdentity(t *testing.T) {
	for _, expired := range []bool{false, true} {
		name, delay := "live", 50*time.Millisecond
		if expired {
			name, delay = "already_expired", -time.Second
		}
		t.Run(name, func(t *testing.T) {
			root, stop := context.WithTimeout(context.Background(), delay)
			defer stop()
			command, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			executor := &contextBlockingExecutor{started: make(chan contextOperation, 1)}
			_, err := WithContext(root, executor).Exec(command, "synthetic")
			if !errors.Is(err, context.DeadlineExceeded) || command.Err() != nil {
				t.Fatalf("root deadline error=%v command=%v", err, command.Err())
			}
			select {
			case op := <-executor.started:
				deadline, ok := op.ctx.Deadline()
				want, _ := root.Deadline()
				if expired || !ok || !deadline.Equal(want) || !errors.Is(op.ctx.Err(), context.DeadlineExceeded) {
					t.Fatalf("root deadline not retained: expired=%v deadline=%v error=%v", expired, deadline, op.ctx.Err())
				}
			default:
				if !expired {
					t.Fatal("live root operation did not reach the executor")
				}
			}
		})
	}
}

func TestContextExecutor_NilExecutorKeepsRealShellCancellation(t *testing.T) {
	root, cancel := context.WithCancel(context.Background())
	defer cancel()
	command, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	result, err := WithContext(root, nil).Exec(command, "/bin/sh", "-c", "printf synthetic")
	if err != nil || result == nil || result.ExitCode != 0 || result.Output != "synthetic" {
		t.Fatalf("nil executor did not retain the real shell: result=%+v err=%v", result, err)
	}
	cancel()
	result, err = WithContext(root, nil).Exec(command, "/bin/sh", "-c", "printf must-not-run")
	if !errors.Is(err, context.Canceled) || result != nil && result.Output != "" {
		t.Fatalf("already canceled root executed a shell: result=%+v err=%v", result, err)
	}
}

type contextPlatformExecutor struct{}

func (contextPlatformExecutor) Exec(ctx context.Context, _ string, args ...string) (*shell.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(args, []string{"platform"}) {
		return nil, fmt.Errorf("unexpected platform command: %q", args)
	}
	return &shell.Result{Output: `{"platform":"merlin","tunnels":[{"id":"wgc1","iface":"vpd-synthetic","connected":true}]}`}, nil
}

func TestRuntime_SubscriptionTunnelUsesNonDefaultTablesPath(t *testing.T) {
	dir := t.TempDir()
	store := NewConfigService(dir, filepath.Join(dir, "data"))
	if err := os.WriteFile(store.ConfigPath(), []byte(`{"advanced":{"tunnel_director":{"mark_shift":20}},"tunnel_director":{"tunnels":{"wgc1":{"clients":["192.168.50.8"]}}},"xray":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	tables := filepath.Join(dir, "non-default-applied-tables")
	if err := os.WriteFile(tables, []byte("2 wgc1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	root, cancel := context.WithCancel(context.Background())
	defer cancel()
	vpn := NewVPNDirectorService(dir, WithContext(root, contextPlatformExecutor{}))
	path, client := subscriptionTunnel(store, vpn, tables)
	if path.ID != "wgc1" || path.Iface != "vpd-synthetic" || path.Mark != 0x00300000 || client == nil {
		t.Fatalf("subscription dial path lost custom table/configured mark: path=%+v client=%v", path, client != nil)
	}
	client.CloseIdleConnections()
}
