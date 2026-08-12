package checks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

// VLESSChecker actually stands up a VLESS tunnel from a caller-supplied
// xray-core config and measures download throughput through it - not just
// "is it reachable" (see TLSChecker for that), but "does this relay
// deliver usable speed from this node's network." See
// docs/superpowers/specs/2026-08-12-vless-speedtest-check-design.md.
//
// Unlike every other Checker in this package, target is just a free-text
// label - the real payload is the config in params, which doesn't fit a
// single target string.
type VLESSChecker struct{}

type vlessParams struct {
	Config json.RawMessage `json:"config"`
}

const (
	vlessSpeedTestURL      = "https://speed.cloudflare.com/__down?bytes=10000000"
	vlessSpeedTestMaxBytes = 10_000_000
	vlessSpeedTestMaxTime  = 15 * time.Second
	vlessStartupTimeout    = 5 * time.Second
	vlessOverallTimeout    = 30 * time.Second
)

func (VLESSChecker) Run(ctx context.Context, netCfg NetConfig, target string, rawParams json.RawMessage) Result {
	var p vlessParams
	if len(rawParams) == 0 {
		msg := "config required: params.config must be a full xray-core config.json"
		return Result{Success: false, ErrorMessage: &msg}
	}
	if err := json.Unmarshal(rawParams, &p); err != nil || len(p.Config) == 0 {
		msg := "config required: params.config must be a full xray-core config.json"
		return Result{Success: false, ErrorMessage: &msg}
	}

	xrayPath, err := ensureXrayBinary()
	if err != nil {
		msg := "xray binary unavailable: " + err.Error()
		return Result{Success: false, ErrorMessage: &msg}
	}

	port, err := freePort()
	if err != nil {
		msg := "could not allocate a local port: " + err.Error()
		return Result{Success: false, ErrorMessage: &msg}
	}

	patchedConfig, err := patchInbound(p.Config, port)
	if err != nil {
		msg := "invalid config: " + err.Error()
		return Result{Success: false, ErrorMessage: &msg}
	}

	configFile, err := writeTempConfig(patchedConfig)
	if err != nil {
		msg := "could not write temp config: " + err.Error()
		return Result{Success: false, ErrorMessage: &msg}
	}
	defer os.Remove(configFile)

	overallCtx, cancel := context.WithTimeout(ctx, vlessOverallTimeout)
	defer cancel()

	cmd := exec.CommandContext(overallCtx, xrayPath, "run", "-c", configFile)
	// Combined, not stderr-only: xray-core writes its own startup/config
	// fatal errors to stdout by default (only access/error logs go where
	// the config's "log" block points them - see patchInbound's doc
	// comment). Watching stderr alone meant a real startup failure showed
	// up as the unhelpful "xray failed to start (no output)" even though
	// xray had, in fact, said exactly why on stdout.
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		msg := "could not start xray: " + err.Error()
		return Result{Success: false, ErrorMessage: &msg}
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()

	if err := waitForPort(overallCtx, port, vlessStartupTimeout); err != nil {
		msg := classifyXrayStartupError(output.String())
		return Result{Success: false, ErrorMessage: &msg}
	}

	mbps, bytesDownloaded, durationMs, err := runSpeedTest(overallCtx, port)
	raw := map[string]any{"bytes_downloaded": bytesDownloaded, "duration_ms": durationMs}
	if err != nil {
		msg := classifyNetError(err)
		return Result{Success: false, ErrorMessage: &msg, Raw: mustJSON(raw)}
	}
	raw["mbps"] = mbps

	return Result{Success: true, Raw: mustJSON(raw)}
}

// freePort asks the OS for an unused local TCP port by briefly binding
// port 0 and reading back what it picked - a fresh port per invocation,
// not a hardcoded constant, since a node can run several VLESS checks
// concurrently (MaxConcurrent, same as every other checker) and two
// simultaneous runs both wanting the same fixed port would collide.
func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// patchInbound replaces config's top-level "inbounds" with exactly one
// SOCKS5 listener on 127.0.0.1:port, discarding whatever inbound(s) the
// caller's config specified (if any) - the agent fully controls the local
// listening side so it's guaranteed to never be reachable from outside
// the node and always at the port the caller (Run, above) already knows
// to dial for the speed test. Every other top-level key (outbounds,
// routing, ...) passes through with its original content unchanged -
// except "log" (see below).
//
// "log" is also forced, for a concrete failure this shipped with: configs
// exported from mobile VPN apps routinely set log.access to a path inside
// that app's own OS sandbox (e.g. an iOS App Group container under
// /private/var/mobile/...), which obviously doesn't exist on whatever node
// runs this check. xray-core fails to open that path at startup and exits
// immediately, before it can even open its SOCKS listener - the caller
// only ever saw "xray failed to start (no output)" (fixed separately by
// capturing combined stdout+stderr in Run, above, but that only makes the
// underlying failure visible, not fixed). A client-exported config's log
// destinations are meaningless on this node regardless, so this always
// wins over whatever the caller supplied - loglevel "warning" is enough to
// still surface genuine config/startup errors on stdout.
func patchInbound(config json.RawMessage, port int) (json.RawMessage, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(config, &doc); err != nil {
		return nil, fmt.Errorf("not valid JSON: %w", err)
	}

	inbound := map[string]any{
		"listen":   "127.0.0.1",
		"port":     port,
		"protocol": "socks",
		"settings": map[string]any{"udp": false},
	}
	inboundsJSON, err := json.Marshal([]any{inbound})
	if err != nil {
		return nil, err
	}
	doc["inbounds"] = inboundsJSON
	doc["log"] = json.RawMessage(`{"loglevel":"warning"}`)

	return json.Marshal(doc)
}

func writeTempConfig(config json.RawMessage) (string, error) {
	f, err := os.CreateTemp("", "pingachock-vless-*.json")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Write(config); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// waitForPort polls addr until something accepts a TCP connection, or ctx
// is done / timeout elapses - xray needs a moment to parse its config and
// bring the SOCKS listener up.
func waitForPort(ctx context.Context, port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	addr := "127.0.0.1:" + strconv.Itoa(port)
	for {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("xray did not start listening within %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// classifyXrayStartupError extracts a short, actionable message from
// xray's own stderr when it fails to start - unlike classifyNetError's
// generic network-failure tokens, a bad VLESS config is something the
// caller can actually fix, so surfacing a trimmed piece of xray's own
// complaint is more useful here than a stable-but-uninformative token.
func classifyXrayStartupError(stderr string) string {
	trimmed := strings.TrimSpace(stderr)
	if trimmed == "" {
		return "xray failed to start (no output)"
	}
	lines := strings.Split(trimmed, "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	const maxLen = 200
	if len(last) > maxLen {
		last = last[:maxLen] + "..."
	}
	return "xray config error: " + last
}

// ensureXrayBinary writes the embedded xray-core binary out to a per-OS
// user cache directory once, and returns its path.
func ensureXrayBinary() (string, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		cacheDir = os.TempDir()
	}
	return ensureXrayBinaryIn(filepath.Join(cacheDir, "pingachock-agent", "xray"))
}

// ensureXrayBinaryIn is ensureXrayBinary with the cache directory as a
// parameter, so tests can point it at a throwaway t.TempDir() instead of
// polluting the real user cache directory with a fake payload.
func ensureXrayBinaryIn(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dest := filepath.Join(dir, embeddedXrayFilename)

	if info, err := os.Stat(dest); err == nil && info.Size() == int64(len(embeddedXrayBinary)) {
		return dest, nil
	}

	tmpFile, err := os.CreateTemp(dir, "xray-*.tmp")
	if err != nil {
		return "", err
	}
	tmp := tmpFile.Name()
	if _, err := tmpFile.Write(embeddedXrayBinary); err != nil {
		tmpFile.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return dest, nil
}

// runSpeedTest downloads through the local SOCKS5 proxy at 127.0.0.1:port
// until either vlessSpeedTestMaxBytes or vlessSpeedTestMaxTime is hit,
// whichever comes first, and returns the measured throughput. The
// download target is a fixed, well-known endpoint - never taken from the
// check's own params - specifically so a VLESS check can't be used to
// make measurement nodes download from an arbitrary caller-chosen URL.
func runSpeedTest(ctx context.Context, port int) (mbps float64, bytesDownloaded int64, durationMs int, err error) {
	dialer, err := proxy.SOCKS5("tcp", "127.0.0.1:"+strconv.Itoa(port), nil, proxy.Direct)
	if err != nil {
		return 0, 0, 0, err
	}
	contextDialer, ok := dialer.(proxy.ContextDialer)
	if !ok {
		return 0, 0, 0, fmt.Errorf("SOCKS5 dialer does not support context")
	}

	client := &http.Client{Transport: &http.Transport{DialContext: contextDialer.DialContext}}

	downloadCtx, cancel := context.WithTimeout(ctx, vlessSpeedTestMaxTime)
	defer cancel()

	req, err := http.NewRequestWithContext(downloadCtx, http.MethodGet, vlessSpeedTestURL, nil)
	if err != nil {
		return 0, 0, 0, err
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, 0, err
	}
	defer resp.Body.Close()

	limited := io.LimitReader(resp.Body, vlessSpeedTestMaxBytes)
	n, copyErr := io.Copy(io.Discard, limited)
	elapsed := time.Since(start)
	durationMs = int(elapsed.Milliseconds())

	// Hitting our own time cap mid-download is success, not failure - we
	// asked for at most vlessSpeedTestMaxTime and got exactly that.
	if copyErr != nil && !errors.Is(copyErr, context.DeadlineExceeded) {
		return 0, n, durationMs, copyErr
	}
	if n == 0 {
		return 0, 0, durationMs, fmt.Errorf("no data received")
	}

	seconds := elapsed.Seconds()
	if seconds <= 0 {
		seconds = 0.001
	}
	mbps = float64(n) * 8 / (seconds * 1_000_000)
	return mbps, n, durationMs, nil
}
