//go:build linux

package netpath

import (
	"syscall"

	"golang.org/x/sys/unix"
)

func tunnelSocketControl(iface string, mark uint32) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, c syscall.RawConn) error {
		var sockErr error
		if err := c.Control(func(fd uintptr) {
			sockErr = unix.BindToDevice(int(fd), iface)
			if sockErr != nil {
				return
			}
			if mark != 0 {
				sockErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, int(mark))
			}
		}); err != nil {
			return err
		}
		return sockErr
	}
}
