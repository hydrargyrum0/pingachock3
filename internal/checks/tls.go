package checks

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"os/exec"
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
		// "timeout" and "connection failed" are ambiguous on their own - a
		// user reported getting a TLS Handshake result "sometimes, sometimes
		// not" with no way to tell why. Both cases mean "never even got a
		// TLS byte back", which happens for two very different reasons: the
		// whole host is silent (down, or ICMP+TCP both filtered), or just
		// this specific port is filtered while the host answers pings fine.
		// A supplementary ICMP probe tells them apart. Skipped when ctx is
		// already past its deadline - a ping run doomed to fail purely from
		// running out of time would misreport as "ip unreachable" for a
		// reason that has nothing to do with the target.
		if (msg == "timeout" || msg == "connection failed") && ctx.Err() == nil {
			msg = diagnoseUnreachable(ctx, netCfg, probeTarget)
		}
		res.ErrorMessage = &msg
	}
	return res
}

// diagnoseUnreachable runs a short supplementary ICMP probe against
// probeTarget and folds the result into one of two stable classification
// tokens - see the call site's doc comment for why this only fires for the
// ambiguous "timeout"/"connection failed" cases.
func diagnoseUnreachable(ctx context.Context, netCfg NetConfig, probeTarget string) string {
	return classifyUnreachable(diagnosticPingReceived(ctx, netCfg, probeTarget))
}

// classifyUnreachable is diagnoseUnreachable's decision logic, split out so
// it's unit-testable without actually shelling out to the OS ping binary -
// mirrors classifyPingError's role in ping.go.
func classifyUnreachable(recv int) string {
	if recv > 0 {
		return "port unreachable"
	}
	return "ip unreachable"
}

// diagnosticPingReceived shells out to the OS ping binary (pingArgs/
// parsePingOutput, same as PingChecker) for a single quick packet and
// returns how many replies came back (0 or 1). Deliberately smaller than
// PingChecker's own defaults (1 packet, 1.5s vs. 4 packets/5s each) - this
// already runs after a failed TLS attempt has burned its own budget, so it
// stays a cheap supplementary probe, not a second full ping check. Just one
// packet also sidesteps Windows ping.exe's fixed ~1s pacing between
// packets, which would otherwise make even a 2-packet probe add a
// second-plus of latency on every ambiguous TLS failure.
func diagnosticPingReceived(ctx context.Context, netCfg NetConfig, target string) int {
	const pingCount = 1
	const pingTimeoutMs = 1500
	overall := time.Duration(pingTimeoutMs)*time.Millisecond*time.Duration(pingCount) + 3*time.Second
	cmdCtx, cancel := context.WithTimeout(ctx, overall)
	defer cancel()

	args := pingArgs(target, pingCount, pingTimeoutMs, netCfg.LocalAddr)
	cmd := exec.CommandContext(cmdCtx, args[0], args[1:]...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	_ = cmd.Run()

	_, recv, _ := parsePingOutput(out.String())
	return recv
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
