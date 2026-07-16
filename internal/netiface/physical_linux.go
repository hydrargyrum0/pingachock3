//go:build linux

package netiface

import "os"

// isPhysical reports whether name is backed by real hardware, using the
// same signal the kernel itself exposes for this: a physical NIC has a
// /sys/class/net/<name>/device symlink pointing at its PCI/USB device.
// Virtual interfaces (tun/tap, WireGuard, veth, bridges, docker0, Tailscale
// and other VPN clients) don't have one - this is the standard way Linux
// tools distinguish real NICs from virtual ones.
func isPhysical(name string) bool {
	_, err := os.Lstat("/sys/class/net/" + name + "/device")
	return err == nil
}
