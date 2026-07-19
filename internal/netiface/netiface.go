// Package netiface lets the operator pick which network interface the agent
// should run checks through, and discovers that interface's own DNS servers
// - deliberately not the system-wide resolver, which can be silently
// overridden by a VPN client. It also flags which interfaces are backed by
// real hardware (IsPhysical) versus a VPN/tunnel/virtual adapter, since
// running checks through a VPN would measure what the VPN's exit node sees,
// not what the local ISP actually does - the whole point of this agent. See
// docs/ARCHITECTURE.md and the `configure` command in cmd/agent.
package netiface

import "net"

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
		if ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
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
			continue
		}
		out = append(out, Interface{
			Name:       ifc.Name,
			Addrs:      ips,
			IsUp:       ifc.Flags&net.FlagUp != 0,
			IsPhysical: isPhysical(ifc.Name),
		})
	}
	return out, nil
}
