//go:build windows

package netiface

import (
	"regexp"
	"strings"

	"os/exec"
)

var ipv4Line = regexp.MustCompile(`^\d{1,3}(\.\d{1,3}){3}$`)

// DNSServers parses `ipconfig /all`. Adapter headers on Windows are
// un-indented lines ending in the adapter's friendly name (the same name
// Go's net.Interfaces() reports), with indented "DNS Servers . . . : <ip>"
// followed by continuation lines that are bare IPs for any additional
// servers.
func DNSServers(ifaceName string) ([]string, error) {
	out, err := exec.Command("ipconfig", "/all").Output()
	if err != nil {
		return nil, err
	}
	return parseIpconfig(string(out), ifaceName), nil
}

func parseIpconfig(output, ifaceName string) []string {
	lines := strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n")

	var inTarget, collecting bool
	var servers []string
	for _, line := range lines {
		if len(line) > 0 && line[0] != ' ' && line[0] != '\t' {
			inTarget = strings.Contains(line, ifaceName)
			collecting = false
			continue
		}
		if !inTarget {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "DNS Servers") {
			collecting = true
			if idx := strings.LastIndex(trimmed, ":"); idx >= 0 {
				if v := strings.TrimSpace(trimmed[idx+1:]); ipv4Line.MatchString(v) {
					servers = append(servers, v)
				}
			}
			continue
		}
		if collecting {
			if ipv4Line.MatchString(trimmed) {
				servers = append(servers, trimmed)
				continue
			}
			collecting = false
		}
	}
	return servers
}
