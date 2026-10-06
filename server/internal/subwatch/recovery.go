package subwatch

import (
	"errors"

	"github.com/zinin/vpn-director/server/internal/vpnconfig"
)

var (
	errRestoreNeedsApply = errors.New("pending restore needs an apply")
	errRestoreApply      = errors.New("pending restore apply failed")
)

func sameRestoreActive(a, b *vpnconfig.ActiveServer) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func pendingRestoreGuard(cfg *vpnconfig.VPNDirectorConfig, expected *vpnconfig.XrayPendingRestore) error {
	if cfg == nil || expected == nil || cfg.Xray.PendingRestore == nil || cfg.Xray.Failover != nil ||
		!sameRestoreActive(cfg.Xray.ActiveServer, expected.Active) || !sameRestoreActive(cfg.Xray.PendingRestore.Active, expected.Active) {
		return errSuperseded
	}
	return nil
}

// reconcileRestore runs with Tick ownership and finishes an interrupted restore.
func (w *Watch) reconcileRestore(cfg *vpnconfig.VPNDirectorConfig) error {
	if cfg == nil || cfg.Xray.PendingRestore == nil {
		return nil
	}
	if err := w.mutationAllowed(); err != nil {
		return err
	}
	if w.UpdateVPN == nil {
		return errors.New("VPN config update unavailable")
	}
	pending := cfg.Xray.PendingRestore
	if !sameRestoreActive(cfg.Xray.ActiveServer, pending.Active) {
		// Discard only metadata for the old selection; the newer assignments stay.
		if err := w.update(func(current *vpnconfig.VPNDirectorConfig) error {
			if current == nil || current.Xray.PendingRestore == nil ||
				!sameRestoreActive(current.Xray.ActiveServer, cfg.Xray.ActiveServer) ||
				!sameRestoreActive(current.Xray.PendingRestore.Active, pending.Active) {
				return errSuperseded
			}
			current.Xray.PendingRestore = nil
			return nil
		}); err != nil {
			return err
		}
		w.pendingApply = false
		w.pendingRestore = nil
		w.resetFail()
		return errSuperseded
	}
	if !w.tproxyReady() {
		_, _, err := w.finalizeRestore(pending, false)
		if !errors.Is(err, errRestoreNeedsApply) {
			return err
		}
	}
	if err := w.apply(); err != nil {
		w.pendingApply = true
		if endsWalk(err) {
			return err
		}
		// A failed apply must not prevent a fresh, normally armed death check.
		if w.LoadVPN != nil {
			current, loadErr := w.LoadVPN()
			if refused := w.mutationAllowed(); refused != nil {
				return refused
			}
			if loadErr != nil {
				return loadErr
			}
			if err := pendingRestoreGuard(current, pending); err != nil {
				return err
			}
		}
		return errors.Join(errRestoreApply, err)
	}
	done, committed, err := w.finalizeRestore(pending, true)
	if err == nil && done && committed {
		w.announceRestored(w.activeLabel(cfg))
	}
	return err
}

// finalizeRestore rechecks readiness, identity and assignments under the config lock.
func (w *Watch) finalizeRestore(expected *vpnconfig.XrayPendingRestore, applied bool) (done, committed bool, err error) {
	if w.UpdateVPN == nil {
		return false, false, errors.New("VPN config update unavailable")
	}
	reinstated := false
	err = w.update(func(current *vpnconfig.VPNDirectorConfig) error {
		if err := pendingRestoreGuard(current, expected); err != nil {
			return err
		}
		ready := w.tproxyReady()
		if err := w.mutationAllowed(); err != nil {
			return err
		}
		if ready && !applied {
			return errRestoreNeedsApply
		}
		pending := current.Xray.PendingRestore
		snapshot := pending.FailoverSnapshot(current)
		if !ready && snapshot != nil {
			vpnconfig.ApplyFailoverSnapshot(current, snapshot)
			reinstated = true
		} else {
			done = ready && applied
			committed = done && snapshot != nil && snapshot.Committed
		}
		current.Xray.PendingRestore = nil
		return nil
	})
	if err != nil {
		w.pendingApply = true
		return false, false, err
	}
	w.pendingApply = false
	w.pendingRestore = nil
	if done {
		w.settled()
		w.resetFallbackState()
		return done, committed, nil
	}
	w.lastTPROXYFail = w.Now()
	if reinstated {
		if err := w.apply(); err != nil {
			w.pendingApply = true
			return false, false, err
		}
	}
	return false, false, nil
}
