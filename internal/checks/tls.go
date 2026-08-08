package checks

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"strconv"
	"time"
)

// TLSChecker times how long a TLS handshake takes against a given
// target:port, over several attempts - built for VPN "link" health checks
// (VLESS/VMess/Trojan-style servers, which are plain TLS underneath). SNI
// is deliberately decoupled from the dialed address, the same trick
// internal/transport/fronted.go already uses for domain fronting: dial the
// VPN server's real IP directly, but present a different (e.g.
// Cloudflare-fronted) hostname in the ClientHello.
type TLSChecker struct{}

type tlsCheckParams struct {
	Port      int    `json:"port"`
	SNI       string `json:"sni"`
	Count     int    `json:"count"`
	TimeoutMs int    `json:"timeout_ms"`
	// AllowInsecure skips certificate verification (chain trust + hostname
	// match against SNI). Defaults to false - full verification, same as
	// internal/transport/fronted.go's existing domain-fronting dial. Worth
	// setting true when dialing a VPN server's real IP directly while
	// presenting an unrelated fronting SNI (e.g. a Cloudflare-routed
	// hostname): that backend's own certificate generally won't match the
	// SNI at all, so strict verification would report the handshake as
	// failed even though the real VPN client - which typically also runs
	// with relaxed verification in exactly this setup - connects fine.
	AllowInsecure bool `json:"allow_insecure"`
}

func (TLSChecker) Run(ctx context.Context, netCfg NetConfig, target string, rawParams json.RawMessage) Result {
	var p tlsCheckParams
	if len(rawParams) > 0 {
		_ = json.Unmarshal(rawParams, &p)
	}
	if p.Port <= 0 {
		p.Port = 443
	}
	if p.Count <= 0 {
		p.Count = 3
	}
	if p.TimeoutMs <= 0 {
		p.TimeoutMs = 5000
	}

	probeTarget, reportedIP := resolveIP(ctx, netCfg.Resolver, target)
	addr := net.JoinHostPort(probeTarget, strconv.Itoa(p.Port))

	sni := chooseSNI(target, p.SNI)

	dialer := net.Dialer{
		Timeout:   time.Duration(p.TimeoutMs) * time.Millisecond,
		LocalAddr: localAddr("tcp", netCfg.LocalAddr),
	}

	var succeeded int
	var totalMs int64
	var lastErr error
	for i := 0; i < p.Count; i++ {
		if ctx.Err() != nil {
			if lastErr == nil {
				lastErr = ctx.Err()
			}
			break
		}
		elapsed, err := tlsHandshakeOnce(ctx, &dialer, addr, sni, p.AllowInsecure)
		if err != nil {
			lastErr = err
			continue
		}
		succeeded++
		totalMs += elapsed.Milliseconds()
	}

	raw := map[string]any{"requests_sent": p.Count, "requests_success": succeeded, "sni": sni}
	if reportedIP != "" {
		raw["resolved_target"] = reportedIP
	}

	res := Result{Success: succeeded > 0, Raw: mustJSON(raw)}
	if succeeded > 0 {
		v := int(totalMs / int64(succeeded))
		res.LatencyMs = &v
	}
	if succeeded == 0 && lastErr != nil {
		msg := lastErr.Error()
		res.ErrorMessage = &msg
	}
	return res
}

// chooseSNI: an explicit sni always wins - that's how the Cloudflare-
// fronting case (dial a specific VPN server IP directly, present a
// Cloudflare-routed hostname) gets configured. Otherwise, it's left empty
// (no SNI sent - matches how a real client behaves connecting straight to
// an IP) unless target itself is a hostname, in which case SNI defaults to
// it, same as any normal TLS client dialing by name.
func chooseSNI(target, explicitSNI string) string {
	if explicitSNI != "" {
		return explicitSNI
	}
	if net.ParseIP(target) == nil {
		return target
	}
	return ""
}

// tlsHandshakeOnce dials addr and completes one TLS handshake presenting
// sni (which may be empty - see the ServerName doc comment on why an empty
// value is a deliberate, valid choice, not a missing one). Returns how long
// the handshake itself took, not including the TCP connect.
func tlsHandshakeOnce(ctx context.Context, dialer *net.Dialer, addr, sni string, allowInsecure bool) (time.Duration, error) {
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	tlsConn := tls.Client(conn, &tls.Config{ServerName: sni, InsecureSkipVerify: allowInsecure})
	start := time.Now()
	err = tlsConn.HandshakeContext(ctx)
	elapsed := time.Since(start)
	if err != nil {
		return 0, err
	}
	return elapsed, nil
}
