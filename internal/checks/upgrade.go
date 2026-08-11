package checks

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"time"
)

// UpgradeChecker probes whether a host answers HTTP 101 Switching Protocols
// to a plaintext Connection: Upgrade request on a port conventionally
// reserved for TLS (443 by default) - the same specific signature
// check_plain_http.py's "upgrade" mode already implements standalone (a
// recon script kept in the repo root, not part of the shipped product).
// See docs/superpowers/specs/2026-08-09-http-101-upgrade-check-design.md.
//
// Deliberately not TLS-wrapped - a plain net.Dialer TCP connection, the
// same dial pattern internal/checks/tcp.go uses, not tls.go's tls.Client.
// The entire point of the check is whether a plaintext request on the
// TLS-conventional port gets upgraded; wrapping it in TLS would test
// something else entirely.
type UpgradeChecker struct{}

type upgradeParams struct {
	Port      int    `json:"port"`
	Protocol  string `json:"protocol"`
	TimeoutMs int    `json:"timeout_ms"`
}

// upgradeMaxBody caps how much of the response is ever read - the status
// line arrives in the first bytes, and there's no reason to trust a
// misbehaving host to ever close the connection. Mirrors
// check_plain_http.py's --max-body default (16 KiB).
const upgradeMaxBody = 16 * 1024

const upgradeUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.6089.3 Safari/537.36"

func (UpgradeChecker) Run(ctx context.Context, netCfg NetConfig, target string, rawParams json.RawMessage) Result {
	var p upgradeParams
	if len(rawParams) > 0 {
		_ = json.Unmarshal(rawParams, &p)
	}
	if p.Port <= 0 {
		p.Port = 443
	}
	if p.Protocol == "" {
		p.Protocol = "websocket"
	}
	if p.TimeoutMs <= 0 {
		p.TimeoutMs = 5000
	}
	timeout := time.Duration(p.TimeoutMs) * time.Millisecond

	probeTarget, reportedIP := resolveIP(ctx, netCfg.Resolver, target, timeout, netCfg.LocalAddr)
	addr := net.JoinHostPort(probeTarget, strconv.Itoa(p.Port))

	raw := map[string]any{"protocol": p.Protocol}
	if reportedIP != "" {
		raw["resolved_target"] = reportedIP
	}

	dialer := net.Dialer{Timeout: timeout, LocalAddr: localAddr("tcp", netCfg.LocalAddr)}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		msg := classifyNetError(err)
		return Result{Success: false, ErrorMessage: &msg, Raw: mustJSON(raw)}
	}
	defer conn.Close()

	deadline := time.Now().Add(timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = conn.SetDeadline(deadline)

	if _, err := conn.Write(buildUpgradeRequest(target, p.Protocol)); err != nil {
		msg := classifyNetError(err)
		return Result{Success: false, ErrorMessage: &msg, Raw: mustJSON(raw)}
	}

	body, err := readUpgradeResponse(conn, upgradeMaxBody)
	if err != nil {
		msg := classifyNetError(err)
		return Result{Success: false, ErrorMessage: &msg, Raw: mustJSON(raw)}
	}

	return Result{Success: matchSwitchingProtocols(body), Raw: mustJSON(raw)}
}

// readUpgradeResponse reads up to maxBody bytes, stopping early on EOF or
// the connection's deadline - mirrors check_plain_http.py's probe(): "took
// what we got" on a timeout, since the status line arrives in the first
// bytes and there's no reason to wait for the full response. Only returns
// an error when literally zero bytes ever arrived - a peer that sends the
// status line and then times out/closes still has enough to check.
func readUpgradeResponse(conn net.Conn, maxBody int) ([]byte, error) {
	var buf bytes.Buffer
	chunk := make([]byte, 4096)
	for buf.Len() < maxBody {
		n, err := conn.Read(chunk)
		if n > 0 {
			buf.Write(chunk[:n])
		}
		if err != nil {
			if buf.Len() > 0 {
				return buf.Bytes(), nil
			}
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// matchSwitchingProtocols reports whether body's HTTP status line is
// "HTTP/x.y 101 ...", mirroring check_plain_http.py's
// match_switching_protocols.
func matchSwitchingProtocols(body []byte) bool {
	statusLine := body
	if i := bytes.IndexByte(body, '\n'); i >= 0 {
		statusLine = body[:i]
	}
	statusLine = bytes.TrimRight(statusLine, "\r")
	parts := bytes.SplitN(statusLine, []byte(" "), 3)
	return len(parts) >= 2 && string(parts[1]) == "101"
}

// buildUpgradeRequest mirrors check_plain_http.py's build_upgrade_request
// for the websocket case - the only protocol actually reachable through
// this checker's fixed defaults (see the design doc), though the function
// stays general so a future caller passing a different protocol still gets
// a well-formed request (Sec-WebSocket-* headers only apply to
// "websocket").
func buildUpgradeRequest(host, protocol string) []byte {
	var b strings.Builder
	b.WriteString("GET / HTTP/1.1\r\n")
	b.WriteString("Host: " + host + "\r\n")
	b.WriteString("User-Agent: " + upgradeUserAgent + "\r\n")
	b.WriteString("Connection: Upgrade\r\n")
	b.WriteString("Upgrade: " + protocol + "\r\n")
	if strings.EqualFold(protocol, "websocket") {
		key := make([]byte, 16)
		_, _ = rand.Read(key)
		b.WriteString("Sec-WebSocket-Version: 13\r\n")
		b.WriteString("Sec-WebSocket-Key: " + base64.StdEncoding.EncodeToString(key) + "\r\n")
	}
	b.WriteString("\r\n")
	return []byte(b.String())
}
