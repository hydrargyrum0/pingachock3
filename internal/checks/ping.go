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
	"strings"
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

	resolvedTarget, reportedIP := resolveIP(ctx, netCfg.Resolver, target)

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
	if sent == 0 {
		sent = p.Count
	}
	success := runErr == nil

	res := Result{
		Success: success,
		Raw: mustJSON(map[string]any{
			"packets_sent": sent, "packets_recv": recv, "output": output, "resolved_target": reportedIP,
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
	// windowsReplyTimeRe pulls each individual reply's round-trip time by
	// anchoring on the literal "TTL=" token, exactly like the recv fallback
	// below - ping.exe never translates it, unlike the "time="/"Average="
	// labels it used to depend on (Windows' "Average = Nms" summary line
	// is display-language text: "Среднее = Nмс" on a Russian box, never
	// matching an English-only regex). That silently left avgMs at 0 and
	// fell through to a several-*second* wall-clock fallback (the full
	// `ping -n 4` invocation, not one packet) on any non-English node -
	// see docs/superpowers/specs/2026-07-25-ping-result-classification-design.md
	// Section B. Matches "...time=2ms TTL=58" and the Cyrillic "...время=2мс
	// TTL=58" the same way, plus Windows' "time<1ms" spelling (captures the
	// "1"). Never matches Unix output: Unix's reply line puts "ttl=" (lower
	// case) *before* the time, not after, so this intentionally leaves
	// Unix's avg to unixAvgRe below.
	windowsReplyTimeRe = regexp.MustCompile(`([\d.]+)\s*(?:ms|мс)?\s*TTL=`)
)

func parsePingOutput(output string) (sent, recv int, avgMs float64) {
	// Pick the regex pair by what's actually in the text, not runtime.GOOS.
	// In production these always agree anyway (the agent only ever parses
	// output from its own OS's ping binary), but branching on GOOS here
	// made the Windows-format case untestable on a non-Windows dev machine
	// or CI runner - it silently took the Unix branch and failed forever,
	// which is exactly how a real regression in windowsStatsRe would have
	// gone unnoticed.
	if m := windowsStatsRe.FindStringSubmatch(output); m != nil {
		sent, _ = strconv.Atoi(m[1])
		recv, _ = strconv.Atoi(m[2])
	} else if m := unixStatsRe.FindStringSubmatch(output); m != nil {
		sent, _ = strconv.Atoi(m[1])
		recv, _ = strconv.Atoi(m[2])
	}
	if avg := averageReplyTimeMs(output); avg > 0 {
		avgMs = avg
	} else if m := unixAvgRe.FindStringSubmatch(output); m != nil {
		avgMs, _ = strconv.ParseFloat(m[1], 64)
	}
	if recv == 0 {
		// "Sent = N, Received = N" (or its Unix equivalent) is
		// locale-dependent text - e.g. on a Russian-locale Windows node
		// ping.exe prints "Отправлено"/"Получено" instead, so the regexes
		// above never match even on a fully successful run. "TTL="/"ttl="
		// isn't translated, so fall back to counting replies by that
		// instead of reporting a false failure.
		recv = strings.Count(strings.ToUpper(output), "TTL=")
	}
	return
}

// averageReplyTimeMs averages every reply's individual round-trip time
// (windowsReplyTimeRe, see its comment) rather than trusting a single
// locale-dependent summary line - also means the average reflects the
// packets that actually came back, not whatever ping.exe's own summary
// line otherwise says. Returns 0 when nothing matched (genuine Unix
// output, or a run with zero replies), so the caller falls back to
// unixAvgRe.
func averageReplyTimeMs(output string) float64 {
	matches := windowsReplyTimeRe.FindAllStringSubmatch(output, -1)
	if len(matches) == 0 {
		return 0
	}
	var sum float64
	for _, m := range matches {
		v, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			continue
		}
		sum += v
	}
	return sum / float64(len(matches))
}
