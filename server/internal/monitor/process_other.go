//go:build !linux

package monitor

import "syscall"

// childAttr has no Pdeathsig outside Linux; the daemon runs on Linux routers
// only, and a workstation build runs the fake prober.
func childAttr() *syscall.SysProcAttr {
	return nil
}
