package service

import (
	"context"

	"github.com/zinin/vpn-director/server/internal/shell"
)

type scopedExecutor struct {
	root     context.Context
	executor ShellExecutor
}

// WithContext keeps command deadlines and also cancels on daemon shutdown.
func WithContext(root context.Context, executor ShellExecutor) ShellExecutor {
	if executor == nil {
		executor = DefaultExecutor()
	}
	return &scopedExecutor{root: root, executor: executor}
}

func (e *scopedExecutor) Exec(ctx context.Context, name string, args ...string) (*shell.Result, error) {
	ctx, cancel := joinOperationContext(e.root, ctx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return e.executor.Exec(ctx, name, args...)
}

func joinOperationContext(root, operation context.Context) (context.Context, context.CancelFunc) {
	if root == nil {
		root = context.Background()
	}
	if operation == nil {
		operation = context.Background()
	}
	parent := operation
	var deadlineCancel context.CancelFunc
	deadline, hasDeadline := root.Deadline()
	if hasDeadline {
		if other, present := operation.Deadline(); !present || deadline.Before(other) {
			parent, deadlineCancel = context.WithDeadline(operation, deadline)
		}
	}
	ctx, cancel := context.WithCancelCause(parent)
	cancelRoot := func() {
		// The parent's deadline supplies DeadlineExceeded; the hook handles early shutdown.
		if root.Err() == context.DeadlineExceeded && hasDeadline {
			return
		}
		cancel(context.Cause(root))
	}
	stop := context.AfterFunc(root, cancelRoot)
	if root.Err() != nil {
		cancelRoot()
	}
	return ctx, func() {
		stop()
		if deadlineCancel != nil {
			deadlineCancel()
		}
		cancel(context.Canceled)
	}
}

// ForContext adds a Tick's lifetime without replacing the daemon's root scope.
func (s *VPNDirectorService) ForContext(ctx context.Context) *VPNDirectorService {
	copy := *s
	copy.executor = WithContext(ctx, s.executor)
	return &copy
}

// ForContext scopes a copy; the shared generator is never mutated by a Tick.
func (s *XrayService) ForContext(ctx context.Context) *XrayService {
	copy := *s
	copy.operation = ctx
	return &copy
}
