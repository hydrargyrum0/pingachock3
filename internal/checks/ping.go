package checks

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"time"
)

// PingChecker shells out to the OS's native ping binary rather than using
// raw ICMP sockets - raw ICMP needs elevated privileges on Windows/macOS/
// Linux in inconsistent ways, while every OS ships a ping binary that
// already handles this correctly and portably.
type PingChecker struct{}

type pingParams struct {
	Count     int `json:"count"`
	TimeoutMs int `json:"timeout_ms"`
}

func (PingChecker) Run(ctx context.Context, netCfg NetConfig, target string, rawParams json.RawMessage) Result {
	var p pingParams
	if len(rawParams) > 0 {
		_ = json.Unmarshal(rawParams, &p)
	}
	if p.Count <= 0 {
		p.Count = 4
	}
	if p.TimeoutMs <= 0 {
		p.TimeoutMs = 5000
	}

	// The OS ping binary does its own DNS resolution internally, using the
	// system resolver - which defeats the point of netCfg.Resolver (e.g. a
	// VPN's DNS silently overriding what "ping example.com" actually
	// tests). So if a resolver is configured and target isn't already an
	// IP, resolve it ourselves first and hand ping the address.
	resolvedTarget := target
	if netCfg.Resolver != nil && net.ParseIP(target) == nil {
		lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if ips, err := netCfg.Resolver.LookupIPAddr(lookupCtx, target); err == nil && len(ips) > 0 {
			resolvedTarget = ips[0].String()
		}
		cancel()
	}

	overall := time.Duration(p.TimeoutMs)*time.Millisecond*time.Duration(p.Count) + 5*time.Second
	cmdCtx, cancel := context.WithTimeout(ctx, overall)
	defer cancel()

	args := pingArgs(resolvedTarget, p.Count, p.TimeoutMs, netCfg.LocalAddr)
	cmd := exec.CommandContext(cmdCtx, args[0], args[1:]...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	start := time.Now()
	runErr := cmd.Run()
	elapsedMs := int(time.Since(start).Milliseconds())

	output := out.String()
	sent, recv, avgMs := parsePingOutput(output)
	success := runErr == nil && recv > 0

	res := Result{
		Success: success,
		Raw: mustJSON(map[string]any{
			"packets_sent": sent, "packets_recv": recv, "output": output, "resolved_target": resolvedTarget,
		}),
	}
	switch {
	case avgMs > 0:
		v := int(avgMs)
		res.LatencyMs = &v
	case success:
		res.LatencyMs = &elapsedMs
	}
	if !success {
		msg := "no reply"
		if runErr != nil {
			msg = runErr.Error()
		}
		res.ErrorMessage = &msg
	}
	return res
}

func pingArgs(target string, count, timeoutMs int, localAddr net.IP) []string {
	if runtime.GOOS == "windows" {
		args := []string{"ping", "-n", strconv.Itoa(count), "-w", strconv.Itoa(timeoutMs)}
		if localAddr != nil {
			args = append(args, "-S", localAddr.String())
		}
		return append(args, target)
	}

	timeoutSec := timeoutMs / 1000
	if timeoutSec < 1 {
		timeoutSec = 1
	}
	args := []string{"ping", "-c", strconv.Itoa(count), "-W", strconv.Itoa(timeoutSec)}
	if localAddr != nil {
		if runtime.GOOS == "darwin" {
			args = append(args, "-S", localAddr.String()) // BSD ping: source address, not interface name
		} else {
			args = append(args, "-I", localAddr.String()) // iputils ping accepts an address here too
		}
	}
	return append(args, target)
}

var (
	unixStatsRe    = regexp.MustCompile(`(\d+) packets transmitted, (\d+)( packets)? received`)
	unixAvgRe      = regexp.MustCompile(`= [\d.]+/([\d.]+)/`)
	windowsStatsRe = regexp.MustCompile(`Sent = (\d+), Received = (\d+)`)
	windowsAvgRe   = regexp.MustCompile(`Average = (\d+)ms`)
)

func parsePingOutput(output string) (sent, recv int, avgMs float64) {
	if runtime.GOOS == "windows" {
		if m := windowsStatsRe.FindStringSubmatch(output); m != nil {
			sent, _ = strconv.Atoi(m[1])
			recv, _ = strconv.Atoi(m[2])
		}
		if m := windowsAvgRe.FindStringSubmatch(output); m != nil {
			v, _ := strconv.Atoi(m[1])
			avgMs = float64(v)
		}
		return
	}
	if m := unixStatsRe.FindStringSubmatch(output); m != nil {
		sent, _ = strconv.Atoi(m[1])
		recv, _ = strconv.Atoi(m[2])
	}
	if m := unixAvgRe.FindStringSubmatch(output); m != nil {
		avgMs, _ = strconv.ParseFloat(m[1], 64)
	}
	return
}
