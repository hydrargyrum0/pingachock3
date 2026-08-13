//go:build linux

package netiface

import (
	"fmt"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// BindControl returns a net.Dialer.Control callback that forces every dial
// through it to leave via the named interface (SO_BINDTODEVICE), the
// interface's existence re-checked fresh on every call - see
// bind_windows.go's doc comment for why this is deliberately never cached.
// No network-byte-order quirk here (unlike Windows' IP_UNICAST_IF):
// SO_BINDTODEVICE takes the interface name directly as a string.
func BindControl(name string) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, c syscall.RawConn) error {
		if _, err := net.InterfaceByName(name); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrInterfaceUnavailable, name, err)
		}

		var sockErr error
		ctrlErr := c.Control(func(fd uintptr) {
			sockErr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, name)
		})
		if ctrlErr != nil {
			return ctrlErr
		}
		return sockErr
	}
}
