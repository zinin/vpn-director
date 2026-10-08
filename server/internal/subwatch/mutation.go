package subwatch

import (
	"context"
	"errors"
	"time"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
	"github.com/zinin/vpn-director/server/internal/watchcompat"
)

func (w *Watch) mutationAllowed() error {
	return w.mutationAllowedContext(w.mutationContext)
}

// mutationRefused is why a mutation may not happen now, with none of the side
// effects of mutationAllowedContext.
func (w *Watch) mutationRefused() error {
	switch {
	case w.stopped():
		return errStopped
	case w.CanMutate != nil:
		return w.CanMutate()
	}
	return nil
}

func (w *Watch) mutationAllowedContext(ctx context.Context) error {
	err := w.mutationRefused()
	if err == nil && ctx != nil {
		err = context.Cause(ctx)
	}
	if err != nil {
		w.mutationFailed.Store(true)
		if ctx != nil && w.mutationCancel != nil {
			w.mutationCancel(err)
		}
	}
	return err
}

func (w *Watch) mutationEnded(ctx context.Context) bool {
	return w.mutationAllowedContext(ctx) != nil
}

func (w *Watch) updateFor(ctx context.Context) func(func(*vpnconfig.VPNDirectorConfig) error) error {
	return func(fn func(*vpnconfig.VPNDirectorConfig) error) error {
		return w.update(func(cfg *vpnconfig.VPNDirectorConfig) error {
			if err := w.mutationAllowedContext(ctx); err != nil {
				return err
			}
			return fn(cfg)
		})
	}
}

func mutationInterrupted(err error) bool {
	return errors.Is(err, errStopped) || errors.Is(err, watchcompat.ErrIncompatible) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (w *Watch) waitAfterRestart(delay time.Duration) {
	ctx := w.mutationContext
	if ctx == nil {
		time.Sleep(delay)
		return
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
