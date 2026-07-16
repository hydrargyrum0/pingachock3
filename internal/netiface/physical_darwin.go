//go:build darwin

package netiface

import (
	"os/exec"
	"strings"
)

// isPhysical reports whether name is a real hardware port (Wi-Fi, Ethernet,
// Thunderbolt Bridge, ...) per `networksetup -listallhardwareports` - VPN/
// tunnel interfaces (utunN - used by IPSec/WireGuard/iCloud Private Relay/
// most VPN clients on macOS - plus ipsecN, ppp0, bridge0, awdl0, llw0,
// gif0, stf0) never appear in that listing, since they aren't hardware.
func isPhysical(name string) bool {
	out, err := exec.Command("networksetup", "-listallhardwareports").Output()
	if err == nil {
		target := "Device: " + name
		for _, line := range strings.Split(string(out), "\n") {
			if strings.TrimSpace(line) == target {
				return true
			}
		}
		return false
	}
	// networksetup unavailable for some reason - fall back to the
	// well-known macOS virtual interface name prefixes, defaulting to
	// "physical" for anything else so a command failure doesn't wrongly
	// exclude every real adapter.
	for _, p := range []string{"utun", "ipsec", "ppp", "gif", "stf", "bridge", "awdl", "llw", "vmnet", "tap", "tun"} {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	return true
}
