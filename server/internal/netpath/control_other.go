//go:build !linux

package netpath

import (
	"fmt"
	"syscall"
)

func tunnelSocketControl(iface string, mark uint32) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, c syscall.RawConn) error {
		return fmt.Errorf("SO_BINDTODEVICE is not supported on this platform (iface %q mark 0x%x)", iface, mark)
	}
}
