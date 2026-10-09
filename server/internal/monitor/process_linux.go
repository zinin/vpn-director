package monitor

import "syscall"

// childAttr kills the prober with the daemon: a prober left behind would hold
// its memory and a port until the next reboot.
func childAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
