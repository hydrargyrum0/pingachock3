//go:build darwin

package netiface

import (
	"os/exec"
	"strings"
)

// DNSServers parses `scutil --dns`, which lists resolver blocks each
// tagged with the interface they apply to (e.g. "if_index : 5 (en0)").
func DNSServers(ifaceName string) ([]string, error) {
	out, err := exec.Command("scutil", "--dns").Output()
	if err != nil {
		return nil, err
	}
	return parseScutilDNS(string(out), ifaceName), nil
}

func parseScutilDNS(output, ifaceName string) []string {
	var servers []string
	marker := "(" + ifaceName + ")"
	for _, block := range strings.Split(output, "resolver #") {
		if !strings.Contains(block, marker) {
			continue
		}
		for _, line := range strings.Split(block, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "nameserver[") {
				continue
			}
			if idx := strings.Index(line, ":"); idx >= 0 {
				servers = append(servers, strings.TrimSpace(line[idx+1:]))
			}
		}
	}
	return dedupe(servers)
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}
