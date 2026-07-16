//go:build windows

package netiface

import (
	"os/exec"
	"strings"
)

// isPhysical reports whether name is a real hardware adapter, via
// PowerShell's `Get-NetAdapter -Physical` - the -Physical switch is backed
// by the adapter's HardwareInterface property and excludes VPN clients,
// Hyper-V/VMware/VirtualBox virtual switches, Bluetooth PAN, and other
// non-hardware adapters by design. Matches on the adapter's friendly name,
// same as net.Interface.Name on Windows and what DNSServers already
// matches against in dns_windows.go.
func isPhysical(name string) bool {
	out, err := exec.Command("powershell", "-NoProfile", "-Command",
		`(Get-NetAdapter -Physical | Select-Object -ExpandProperty Name) -join "`+"`n"+`"`,
	).Output()
	if err != nil {
		// Best-effort: don't wrongly exclude every interface if
		// PowerShell/the NetAdapter module is unavailable.
		return true
	}
	for _, line := range strings.Split(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == name {
			return true
		}
	}
	return false
}
