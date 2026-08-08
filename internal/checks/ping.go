package checks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

	resolvedTarget, reportedIP := resolveIP(ctx, netCfg.Resolver, target, time.Duration(p.TimeoutMs)*time.Millisecond, netCfg.LocalAddr)
	resolutionFailed := net.ParseIP(target) == nil && reportedIP == ""

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
	// Only trust "the stats line just didn't parse" (locale mismatch, see
	// parsePingOutput) as a reason to assume the full p.Count actually went
	// out. When we already know resolution itself failed - domain target,
	// resolveIP came back empty - a run that never sent a single packet
	// would otherwise get misreported as "p.Count sent, 0 received", i.e. a
	// 100% loss run that never happened. See
	// docs/superpowers/specs/2026-07-25-ping-result-classification-design.md
	// Section B.
	if sent == 0 && !resolutionFailed {
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
		msg := classifyPingError(cmdCtx.Err(), resolutionFailed, recv)
		res.ErrorMessage = &msg
	}
	return res
}

// classifyPingError turns a shelled-out ping's exit error into a short,
// stable classification instead of leaking the raw os/exec text (almost
// always just "exit status 1" or "exit status 2", regardless of *why* -
// ping's exit code alone never says whether the failure was DNS resolution,
// a timeout, or a genuine no-reply). See
// docs/superpowers/specs/2026-07-25-ping-result-classification-design.md
// Section B item 1.
func classifyPingError(cmdCtxErr error, resolutionFailed bool, recv int) string {
	switch {
	case errors.Is(cmdCtxErr, context.DeadlineExceeded):
		return "timeout"
	case resolutionFailed:
		return "dns resolution failed"
	case recv == 0:
		return "no reply"
	default:
		return "ping failed"
	}
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
	// windowsReplyTimeNoTTLRe covers Windows IPv6 replies, which never
	// include a TTL= field at all (ping.exe just omits hop-limit reporting
	// for IPv6) - e.g. "Reply from 2606:4700:4700::1111: time=15ms" with
	// nothing after it, so windowsReplyTimeRe's TTL= anchor finds nothing.
	// Anchored on the "Reply from"/"Ответ от" line prefix (the same
	// bilingual pair parsePingOutput's Windows-format detection already
	// relies on) specifically so it stays safe to try unconditionally:
	// Unix's reply line ("64 bytes from X: icmp_seq=1 ttl=118 time=13.5
	// ms") also contains "time=", but never "Reply from"/"Ответ от", so
	// this can never misfire against genuine Unix output the way a bare
	// "time=" grep would.
	windowsReplyTimeNoTTLRe = regexp.MustCompile(`(?:Reply from|Ответ от)[^\r\n]*?(?:time|время)[=<]([\d.]+)\s*(?:ms|мс)`)
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
	// Each of these three is safe to try unconditionally and in this order,
	// regardless of locale or OS - see each regex's own doc comment for why
	// it can't misfire against a format it isn't meant for:
	// TTL-anchored Windows reply time, then the no-TTL Windows IPv6
	// fallback, then Unix's summary-line average.
	if avg := averageReplyTimeMs(output); avg > 0 {
		avgMs = avg
	} else if avg := averageReplyTimeMsNoTTL(output); avg > 0 {
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
	return averageMatches(windowsReplyTimeRe.FindAllStringSubmatch(output, -1))
}

// averageReplyTimeMsNoTTL is averageReplyTimeMs's fallback for Windows IPv6
// replies (see windowsReplyTimeNoTTLRe) - only reached once averageReplyTimeMs
// already found nothing, i.e. either the run had zero replies or none of
// them had a TTL= field to anchor on.
func averageReplyTimeMsNoTTL(output string) float64 {
	return averageMatches(windowsReplyTimeNoTTLRe.FindAllStringSubmatch(output, -1))
}

func averageMatches(matches [][]string) float64 {
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
