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
	//
	// Has no effect when sni ends up empty (a raw-IP target with no
	// explicit sni) - see tlsConfigFor's doc comment for why that case
	// always skips verification regardless of this flag.
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
	timeout := time.Duration(p.TimeoutMs) * time.Millisecond

	probeTarget, reportedIP := resolveIP(ctx, netCfg.Resolver, target, timeout, netCfg.LocalAddr)
	addr := net.JoinHostPort(probeTarget, strconv.Itoa(p.Port))

	sni := chooseSNI(target, p.SNI)

	dialer := net.Dialer{
		Timeout:   timeout,
		LocalAddr: localAddr("tcp", netCfg.LocalAddr),
	}

	var attempts, succeeded int
	var totalMs int64
	var lastErr error
	for i := 0; i < p.Count; i++ {
		if ctx.Err() != nil {
			if lastErr == nil {
				lastErr = ctx.Err()
			}
			break
		}
		attempts++

		// Each attempt gets its own bounded deadline covering dial *and*
		// handshake. dialer.Timeout above only ever bounded the TCP
		// connect; HandshakeContext was previously handed the caller's raw
		// ctx, which in production (internal/serveragent.Runner) has no
		// deadline at all - a peer that accepts the TCP connection but
		// stalls mid-handshake (a plausible censoring-middlebox behavior)
		// would hang forever, permanently occupying one of the runner's
		// concurrency slots. See
		// docs/superpowers/specs/2026-07-25-ping-result-classification-design.md.
		attemptCtx, cancel := context.WithTimeout(ctx, timeout)
		elapsed, err := tlsHandshakeOnce(attemptCtx, &dialer, addr, sni, p.AllowInsecure)
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		succeeded++
		totalMs += elapsed.Milliseconds()
	}

	raw := map[string]any{"requests_sent": attempts, "requests_success": succeeded, "sni": sni}
	if reportedIP != "" {
		raw["resolved_target"] = reportedIP
	}

	res := Result{Success: succeeded > 0, Raw: mustJSON(raw)}
	if succeeded > 0 {
		v := int(totalMs / int64(succeeded))
		res.LatencyMs = &v
	}
	if succeeded == 0 && lastErr != nil {
		msg := classifyNetError(lastErr)
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

// tlsConfigFor builds the tls.Config for one handshake attempt. When sni is
// empty (a raw-IP target with no explicit SNI - see chooseSNI), certificate
// verification is always skipped regardless of allowInsecure: Go's
// crypto/tls refuses to even attempt a handshake with ServerName=="" and
// InsecureSkipVerify=false, returning "tls: either ServerName or
// InsecureSkipVerify must be specified" before any network I/O - which used
// to make every raw-IP TLS check fail unconditionally, the exact case this
// checker's own doc comment claims to support. There is no meaningful
// hostname to verify a certificate against when no SNI was sent in the
// first place, so skipping verification here isn't a real weakening - it's
// the only way to exercise "does a handshake complete at all" against a
// bare IP, which is what a VPN link timing check cares about.
func tlsConfigFor(sni string, allowInsecure bool) *tls.Config {
	return &tls.Config{ServerName: sni, InsecureSkipVerify: allowInsecure || sni == ""}
}

// tlsHandshakeOnce dials addr and completes one TLS handshake presenting
// sni (which may be empty - see tlsConfigFor's doc comment on why an empty
// value is a deliberate, valid choice, not a missing one). Returns how long
// the handshake itself took, not including the TCP connect.
func tlsHandshakeOnce(ctx context.Context, dialer *net.Dialer, addr, sni string, allowInsecure bool) (time.Duration, error) {
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	tlsConn := tls.Client(conn, tlsConfigFor(sni, allowInsecure))
	start := time.Now()
	err = tlsConn.HandshakeContext(ctx)
	elapsed := time.Since(start)
	if err != nil {
		return 0, err
	}
	return elapsed, nil
}
