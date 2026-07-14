//go:build linux

package netiface

import (
	"bufio"
	"os"
	"os/exec"
	"strings"
)

// DNSServers tries systemd-resolved first (the common case on modern
// Debian/Ubuntu, and the only way to get a genuinely per-interface answer),
// falling back to the system-wide /etc/resolv.conf if that's unavailable -
// which is a best-effort fallback, not truly scoped to ifaceName.
func DNSServers(ifaceName string) ([]string, error) {
	if out, err := exec.Command("resolvectl", "dns", ifaceName).Output(); err == nil {
		if servers := parseResolvectl(string(out)); len(servers) > 0 {
			return servers, nil
		}
	}
	return systemResolvConf()
}

func parseResolvectl(out string) []string {
	// e.g. "Link 2 (eth0): 192.168.1.1 8.8.8.8"
	idx := strings.Index(out, ":")
	if idx < 0 {
		return nil
	}
	return strings.Fields(out[idx+1:])
}

func systemResolvConf() ([]string, error) {
	f, err := os.Open("/etc/resolv.conf")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var servers []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if after, ok := strings.CutPrefix(line, "nameserver"); ok {
			servers = append(servers, strings.TrimSpace(after))
		}
	}
	return servers, sc.Err()
}
