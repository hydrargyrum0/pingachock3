// Package checks implements the actual network probes a node runs. Each
// check type is a Checker registered by name - adding a new type means
// adding one file and one registry entry, nothing else changes.
// See docs/ARCHITECTURE.md "Структура Go-агента".
package checks

import (
	"context"
	"encoding/json"
	"net"
	"time"
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

// resolveIP resolves target with resolver (falling back to the default
// system resolver when resolver is nil - i.e. no custom interface/DNS was
// configured), unless target is already a literal IP. probeTarget is what
// the caller should actually connect/ping to; reportedIP is the same value
// when a lookup happened, or "" when target was already an IP or the
// lookup failed (nothing new to report).
//
// Two things this buys callers over leaving resolution to the OS ping
// binary or net.Dialer's own internal lookup: (a) a custom resolver (e.g.
// a VPN's DNS) actually gets used, rather than being silently ignored in
// favor of the system resolver; (b) the caller learns which IP a domain
// resolved to at all - needed to report it (the bot's DNS-poisoning
// classification hinges on knowing a domain resolved to 127.0.0.1, see
// docs/superpowers/specs/2026-07-25-ping-result-classification-design.md)
// - and probing that exact address avoids a second, possibly different,
// resolution happening inside ping/Dialer (round-robin DNS).
func resolveIP(ctx context.Context, resolver *net.Resolver, target string) (probeTarget, reportedIP string) {
	if net.ParseIP(target) != nil {
		return target, ""
	}
	r := resolver
	if r == nil {
		r = net.DefaultResolver
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ips, err := r.LookupIPAddr(lookupCtx, target)
	if err != nil || len(ips) == 0 {
		return target, ""
	}
	ip := ips[0].String()
	return ip, ip
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
