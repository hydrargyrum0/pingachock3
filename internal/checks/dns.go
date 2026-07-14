package checks

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"
)

type DNSChecker struct{}

type dnsParams struct {
	RecordType string `json:"record_type"`
	Resolver   string `json:"resolver"`
	TimeoutMs  int    `json:"timeout_ms"`
}

func (DNSChecker) Run(ctx context.Context, netCfg NetConfig, target string, rawParams json.RawMessage) Result {
	var p dnsParams
	if len(rawParams) > 0 {
		_ = json.Unmarshal(rawParams, &p)
	}
	if p.RecordType == "" {
		p.RecordType = "A"
	}
	if p.TimeoutMs <= 0 {
		p.TimeoutMs = 5000
	}

	// Explicit resolver param on the check wins (operator deliberately
	// wants to query e.g. 8.8.8.8); otherwise default to the interface's
	// own DNS servers from `configure`, if any; otherwise system default.
	resolver := netCfg.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	if p.Resolver != "" {
		resolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				d := net.Dialer{Timeout: time.Duration(p.TimeoutMs) * time.Millisecond, LocalAddr: localAddr(network, netCfg.LocalAddr)}
				return d.DialContext(ctx, network, net.JoinHostPort(p.Resolver, "53"))
			},
		}
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(p.TimeoutMs)*time.Millisecond)
	defer cancel()

	start := time.Now()
	answers, err := lookup(ctx, resolver, strings.ToUpper(p.RecordType), target)
	elapsed := int(time.Since(start).Milliseconds())

	if err != nil {
		msg := err.Error()
		return Result{Success: false, LatencyMs: &elapsed, ErrorMessage: &msg}
	}
	return Result{Success: len(answers) > 0, LatencyMs: &elapsed, Raw: mustJSON(map[string]any{"answers": answers})}
}

func lookup(ctx context.Context, resolver *net.Resolver, recordType, target string) ([]string, error) {
	switch recordType {
	case "A", "AAAA":
		ips, err := resolver.LookupIPAddr(ctx, target)
		if err != nil {
			return nil, err
		}
		out := make([]string, len(ips))
		for i, ip := range ips {
			out[i] = ip.String()
		}
		return out, nil
	case "CNAME":
		cname, err := resolver.LookupCNAME(ctx, target)
		if err != nil {
			return nil, err
		}
		return []string{cname}, nil
	case "MX":
		mx, err := resolver.LookupMX(ctx, target)
		if err != nil {
			return nil, err
		}
		out := make([]string, len(mx))
		for i, m := range mx {
			out[i] = fmt.Sprintf("%s %d", m.Host, m.Pref)
		}
		return out, nil
	case "TXT":
		return resolver.LookupTXT(ctx, target)
	case "NS":
		ns, err := resolver.LookupNS(ctx, target)
		if err != nil {
			return nil, err
		}
		out := make([]string, len(ns))
		for i, n := range ns {
			out[i] = n.Host
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported record_type: %s", recordType)
	}
}
