// Package checks implements the actual network probes a node runs. Each
// check type is a Checker registered by name - adding a new type means
// adding one file and one registry entry, nothing else changes.
// See docs/ARCHITECTURE.md "Структура Go-агента".
package checks

import (
	"context"
	"encoding/json"
	"net"
)

type Result struct {
	Success      bool
	LatencyMs    *int
	StatusCode   *string
	ErrorMessage *string
	Raw          json.RawMessage
}

// NetConfig pins checks to a specific network interface, set by the
// operator via `configure` (see internal/netiface). LocalAddr is nil and
// Resolver is nil when no interface was selected - checkers then fall back
// to whatever the OS/Go default would do, unchanged from before this
// existed.
type NetConfig struct {
	LocalAddr net.IP
	Resolver  *net.Resolver
}

type Checker interface {
	Run(ctx context.Context, netCfg NetConfig, target string, params json.RawMessage) Result
}

var registry = map[string]Checker{
	"ping": PingChecker{},
	"tcp":  TCPChecker{},
	"http": HTTPChecker{},
	"dns":  DNSChecker{},
}

func Get(checkType string) (Checker, bool) {
	c, ok := registry[checkType]
	return c, ok
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

// localAddr builds the right net.Addr type for the given network ("tcp...",
// "udp...") - net.Dialer.LocalAddr must match the dial network's address
// family or the dial fails outright.
func localAddr(network string, ip net.IP) net.Addr {
	if ip == nil {
		return nil
	}
	switch {
	case len(network) >= 3 && network[:3] == "tcp":
		return &net.TCPAddr{IP: ip}
	case len(network) >= 3 && network[:3] == "udp":
		return &net.UDPAddr{IP: ip}
	default:
		return &net.IPAddr{IP: ip}
	}
}
