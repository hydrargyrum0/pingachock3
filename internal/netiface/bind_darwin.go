//go:build darwin

package netiface

import (
	"fmt"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// BindControl returns a net.Dialer.Control callback that forces every dial
// through it to leave via the named interface (IP_BOUND_IF, by index - no
// network-byte-order quirk here, unlike Windows' IP_UNICAST_IF), the
// interface re-checked fresh on every call - see bind_windows.go's doc
// comment for why this is deliberately never cached.
func BindControl(name string) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, c syscall.RawConn) error {
		ifc, err := net.InterfaceByName(name)
		if err != nil {
			return fmt.Errorf("%w: %s: %v", ErrInterfaceUnavailable, name, err)
		}

		var sockErr error
		ctrlErr := c.Control(func(fd uintptr) {
			sockErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_BOUND_IF, ifc.Index)
		})
		if ctrlErr != nil {
			return ctrlErr
		}
		return sockErr
	}
}
