// Package checks implements the actual network probes a node runs. Each
// check type is a Checker registered by name - adding a new type means
// adding one file and one registry entry, nothing else changes.
// See docs/ARCHITECTURE.md "Структура Go-агента".
package checks

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"syscall"
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
	"ping":    PingChecker{},
	"tcp":     TCPChecker{},
	"http":    HTTPChecker{},
	"dns":     DNSChecker{},
	"tls":     TLSChecker{},
	"upgrade": UpgradeChecker{},
	"vless":   VLESSChecker{},
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
//
// timeout bounds the lookup itself - callers pass their own configured
// check timeout rather than this function assuming a fixed budget, so DNS
// resolution can't eat into a tight timeout_ms unnoticed (a slow resolver
// used to be able to block up to a hardcoded 5s here alone, regardless of
// what the check itself was configured to allow).
//
// When the lookup returns multiple addresses, an IPv4 one is preferred
// (mirroring internal/netiface.PreferredAddr's reasoning: IPv6 connectivity
// on a network being measured for reachability is far more likely to be
// broken/unroutable than IPv4, and a broken address chosen here means the
// whole check fails even though the target is genuinely reachable). When
// preferFamily is set (a NetConfig.LocalAddr is pinning the check to a
// specific interface), an address of that same family is preferred instead
// - dialing/pinging with a mismatched family fails at the socket layer
// before ever reaching the network. This is a best-effort single choice,
// not full Happy-Eyeballs multi-address retry: if the chosen address turns
// out to be unroutable for a reason other than family mismatch, the check
// still just fails, the same as before this preference existed.
func resolveIP(ctx context.Context, resolver *net.Resolver, target string, timeout time.Duration, preferFamily net.IP) (probeTarget, reportedIP string) {
	if net.ParseIP(target) != nil {
		return target, ""
	}
	r := resolver
	if r == nil {
		r = net.DefaultResolver
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	lookupCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ips, err := r.LookupIPAddr(lookupCtx, target)
	if err != nil || len(ips) == 0 {
		return target, ""
	}
	ip := pickPreferredIP(ips, preferFamily).String()
	return ip, ip
}

// ResolveIP is resolveIP exported for callers outside this package (the
// /api/v1/server-ping handler) that need to resolve a target once and reuse
// the exact same address across several checks against it, rather than
// letting each check independently resolve and race to report whichever
// answer happened to come back first - see internal/api/public/serverping.go.
func ResolveIP(ctx context.Context, resolver *net.Resolver, target string, timeout time.Duration) (probeTarget, reportedIP string) {
	return resolveIP(ctx, resolver, target, timeout, nil)
}

// pickPreferredIP returns the first address matching preferFamily's address
// family if one is set and present, else the first IPv4 address if any are
// present, else simply ips[0] (e.g. an IPv6-only result).
func pickPreferredIP(ips []net.IPAddr, preferFamily net.IP) net.IP {
	if preferFamily != nil {
		wantV4 := preferFamily.To4() != nil
		for _, ip := range ips {
			if (ip.IP.To4() != nil) == wantV4 {
				return ip.IP
			}
		}
	}
	for _, ip := range ips {
		if ip.IP.To4() != nil {
			return ip.IP
		}
	}
	return ips[0].IP
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

// classifyNetError maps a connection-level error (TCP dial, TLS handshake)
// to a short, stable, English classification instead of leaking Go's raw
// error text - e.g. "dial tcp 1.2.3.4:443: connect: connection refused" -
// straight through to API consumers and, from there, to bot users. See
// docs/superpowers/specs/2026-07-25-ping-result-classification-design.md
// Section B item 1. Callers/consumers (like the bot) are expected to
// translate these stable tokens into whatever display language they want;
// this package stays language-neutral since it's also the public API's
// ErrorMessage contract (internal/api/openapi.yaml).
func classifyNetError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "connection refused"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns resolution failed"
	}
	var hostErr x509.HostnameError
	var authErr x509.UnknownAuthorityError
	var certErr x509.CertificateInvalidError
	if errors.As(err, &hostErr) || errors.As(err, &authErr) || errors.As(err, &certErr) {
		return "certificate verification failed"
	}
	return "connection failed"
}
