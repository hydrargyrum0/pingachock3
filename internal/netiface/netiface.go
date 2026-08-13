// Package netiface lets the operator pick which network interface the agent
// should run checks through, and discovers that interface's own DNS servers
// - deliberately not the system-wide resolver, which can be silently
// overridden by a VPN client. It also flags which interfaces are backed by
// real hardware (IsPhysical) versus a VPN/tunnel/virtual adapter, since
// running checks through a VPN would measure what the VPN's exit node sees,
// not what the local ISP actually does - the whole point of this agent. See
// docs/ARCHITECTURE.md and the `configure` command in cmd/agent.
package netiface

import (
	"errors"
	"fmt"
	"net"
)

// ErrInterfaceUnavailable wraps every error this package returns when a
// pinned interface no longer exists (removed adapter, cable unplugged,
// Wi-Fi turned off) - checkers unwrap it with errors.Is to report a
// distinct, actionable classification instead of it looking like every
// check target on earth suddenly went down. See
// docs/superpowers/specs/2026-08-13-vpn-resilient-node-networking-design.md
// Part 3.
var ErrInterfaceUnavailable = errors.New("network interface unavailable")

type Interface struct {
	Name       string
	Addrs      []net.IP
	IsUp       bool
	IsPhysical bool
}

// PreferredAddr picks which of the interface's addresses to hand checks as
// their source address. Prefers IPv4: net.Dialer.LocalAddr (and ping's -S)
// must share an address family with the destination or the dial/ping fails
// outright before a single packet goes out, and check targets are
// overwhelmingly IPv4 - so an IPv6 address picked here would silently break
// every IPv4 check. Falls back to the first address (typically IPv6) if the
// interface has no IPv4 address at all.
func (i Interface) PreferredAddr() net.IP {
	if len(i.Addrs) == 0 {
		return nil
	}
	for _, a := range i.Addrs {
		if a.To4() != nil {
			return a
		}
	}
	return i.Addrs[0]
}

// List returns non-loopback interfaces that have at least one non-link-local
// address, since those are the only ones useful to route checks through.
func List() ([]Interface, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}

	var out []Interface
	for _, ifc := range ifaces {
		converted, ok := fromNetInterface(ifc)
		if !ok {
			continue
		}
		out = append(out, converted)
	}
	return out, nil
}

// ByName looks up a single interface's *current* state fresh, every call -
// unlike List, which is for presenting the whole choice to an operator at
// `configure` time, this is for checkers that need this interface's
// present-moment address right before using it (PingChecker, shelling out
// to the OS ping binary; VLESSChecker, building an xray-core config's
// sendThrough field) because they can't use netiface's Control-based
// BindControl the way every other checker does. Callers must call this
// fresh on every use, never cache the result across check runs, or they
// reintroduce exactly the staleness bug this whole package exists to fix.
// See docs/superpowers/specs/2026-08-13-vpn-resilient-node-networking-design.md.
func ByName(name string) (Interface, error) {
	ifc, err := net.InterfaceByName(name)
	if err != nil {
		return Interface{}, fmt.Errorf("%w: %s: %v", ErrInterfaceUnavailable, name, err)
	}
	converted, ok := fromNetInterface(*ifc)
	if !ok {
		return Interface{}, fmt.Errorf("%w: %s: interface has no usable address", ErrInterfaceUnavailable, name)
	}
	return converted, nil
}

// fromNetInterface converts a stdlib net.Interface into our Interface,
// applying the same filtering List has always applied (skip loopback, skip
// interfaces whose only addresses are link-local) - ok is false when ifc
// should be skipped entirely.
func fromNetInterface(ifc net.Interface) (Interface, bool) {
	if ifc.Flags&net.FlagLoopback != 0 {
		return Interface{}, false
	}
	addrs, err := ifc.Addrs()
	if err != nil {
		return Interface{}, false
	}
	var ips []net.IP
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip == nil || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			continue
		}
		ips = append(ips, ip)
	}
	if len(ips) == 0 {
		return Interface{}, false
	}
	return Interface{
		Name:       ifc.Name,
		Addrs:      ips,
		IsUp:       ifc.Flags&net.FlagUp != 0,
		IsPhysical: isPhysical(ifc.Name),
	}, true
}
