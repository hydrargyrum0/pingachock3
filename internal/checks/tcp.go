package checks

import (
	"context"
	"encoding/json"
	"net"
	"strconv"
	"time"
)

type TCPChecker struct{}

type tcpParams struct {
	Port      int `json:"port"`
	TimeoutMs int `json:"timeout_ms"`
}

func (TCPChecker) Run(ctx context.Context, netCfg NetConfig, target string, rawParams json.RawMessage) Result {
	var p tcpParams
	if len(rawParams) > 0 {
		_ = json.Unmarshal(rawParams, &p)
	}
	if p.Port <= 0 {
		p.Port = 443
	}
	if p.TimeoutMs <= 0 {
		p.TimeoutMs = 5000
	}

	// Resolve up front rather than leaving it to net.Dialer's internal
	// lookup - same reasoning as internal/checks/ping.go's resolveIP call:
	// makes a custom resolver actually take effect, and lets the result
	// report which IP the domain resolved to (resolved_target below).
	probeTarget, reportedIP := resolveIP(ctx, netCfg.Resolver, target)
	addr := net.JoinHostPort(probeTarget, strconv.Itoa(p.Port))
	dialer := net.Dialer{
		Timeout:   time.Duration(p.TimeoutMs) * time.Millisecond,
		Resolver:  netCfg.Resolver,
		LocalAddr: localAddr("tcp", netCfg.LocalAddr),
	}

	raw := map[string]any{"address": addr}
	if reportedIP != "" {
		raw["resolved_target"] = reportedIP
	}

	start := time.Now()
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	elapsed := int(time.Since(start).Milliseconds())

	if err != nil {
		msg := err.Error()
		return Result{Success: false, ErrorMessage: &msg, Raw: mustJSON(raw)}
	}
	conn.Close()
	return Result{Success: true, LatencyMs: &elapsed, Raw: mustJSON(raw)}
}
