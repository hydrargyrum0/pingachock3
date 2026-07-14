// Package netiface lets the operator pick which network interface the agent
// should run checks through, and discovers that interface's own DNS servers
// - deliberately not the system-wide resolver, which can be silently
// overridden by a VPN client. See docs/ARCHITECTURE.md and the `configure`
// command in cmd/agent.
package netiface

import "net"

type Interface struct {
	Name  string
	Addrs []net.IP
	IsUp  bool
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
			Name:  ifc.Name,
			Addrs: ips,
			IsUp:  ifc.Flags&net.FlagUp != 0,
		})
	}
	return out, nil
}
