//go:build windows

package netiface

import (
	"encoding/binary"
	"fmt"
	"net"
	"syscall"

	"golang.org/x/sys/windows"
)

// ipUnicastIF is IP_UNICAST_IF (ws2ipdef.h, value 31) - not predefined in
// golang.org/x/sys/windows, so declared here. Forces IPv4 unicast egress
// through a specific interface regardless of what the routing table would
// otherwise prefer - the same mechanism VPN clients rely on to claim the
// default route, used here in reverse so a check's traffic leaves through
// the operator-pinned physical adapter instead of whatever a VPN/tunnel
// currently prefers. IPv6 has its own IPV6_UNICAST_IF option under
// IPPROTO_IPV6; not implemented here, matching this codebase's existing
// IPv4-first stance elsewhere (see internal/checks.pickPreferredIP) - an
// IPv6 dial through a pinned interface falls back to whatever the OS
// default route does for IPv6, unchanged from before this package existed.
const ipUnicastIF = 31

// BindControl returns a net.Dialer.Control callback that forces every dial
// through it to leave via the named interface. The interface is resolved
// fresh on every single call (never a cached index) - both so a
// DHCP-renewed address never matters (the index survives a renewal; only
// resolving it fresh matters for the case the interface itself is gone -
// see net.InterfaceByName's error below) and so a removed interface fails
// loudly and immediately with ErrInterfaceUnavailable, rather than
// silently falling back to whatever the OS would otherwise pick. See
// docs/superpowers/specs/2026-08-13-vpn-resilient-node-networking-design.md.
func BindControl(name string) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, c syscall.RawConn) error {
		ifc, err := net.InterfaceByName(name)
		if err != nil {
			return fmt.Errorf("%w: %s: %v", ErrInterfaceUnavailable, name, err)
		}

		// IP_UNICAST_IF is one of the few Windows socket options that wants
		// its DWORD value in network byte order, not host byte order - a
		// well-documented, easy-to-get-wrong quirk specific to this option.
		be := make([]byte, 4)
		binary.BigEndian.PutUint32(be, uint32(ifc.Index))
		indexNetworkOrder := int(binary.LittleEndian.Uint32(be))

		var sockErr error
		ctrlErr := c.Control(func(fd uintptr) {
			sockErr = windows.SetsockoptInt(windows.Handle(fd), windows.IPPROTO_IP, ipUnicastIF, indexNetworkOrder)
		})
		if ctrlErr != nil {
			return ctrlErr
		}
		return sockErr
	}
}
