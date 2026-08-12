# VLESS Config Speed Test Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Given a full xray-core VLESS config, stand up that tunnel on a real node and measure download throughput through it, with xray-core embedded into the agent/backend binaries at build time (never downloaded at runtime).

**Architecture:** A build-time fetch script pulls the pinned xray-core release for each target platform; six tiny `//go:embed` files (one per platform, gated by build tags) expose the matching binary as a package-level `[]byte`; a new `internal/checks.VLESSChecker` extracts it to a cache dir once, patches the caller's config to add a private local SOCKS5 inbound, runs xray as a subprocess, and speed-tests through it against a fixed endpoint. Dispatched through the exact same `/api/v1/checks` + `node_selector` mechanism every other node-routed check already uses - no new backend endpoint.

**Tech Stack:** Go (`os/exec`, `//go:embed`, `golang.org/x/net/proxy` for the SOCKS5 dial), xray-core v26.3.27 (external binary, embedded not vendored), TypeScript/Telegraf (bot).

**Spec:** `docs/superpowers/specs/2026-08-12-vless-speedtest-check-design.md`

**Scope note (stated plainly, not hidden in a code comment):** the bot UI in Task 5 only accepts the config pasted as a text message, not an uploaded `.json` file. Telegram's message limit (4096 chars) comfortably fits a typical VLESS config; document-upload support can be added later as a small, isolated follow-up if a config ever doesn't fit. Also matching the TLS Handshake Check design's own precedent: node selection is `auto` or one specific node, no `ALL` fan-out.

---

## Task 1: xray-core fetch tooling + embedding scaffolding

**Files:**
- Create: `scripts/fetch-xray.sh`
- Create: `internal/checks/xray_windows_amd64.go`
- Create: `internal/checks/xray_windows_386.go`
- Create: `internal/checks/xray_linux_amd64.go`
- Create: `internal/checks/xray_linux_arm64.go`
- Create: `internal/checks/xray_darwin_amd64.go`
- Create: `internal/checks/xray_darwin_arm64.go`
- Modify: `.gitignore`

This task has no unit tests of its own - it's build tooling and embedding
scaffolding. It's verified by successfully running `go build ./...`
afterward (proves the embed directives resolve correctly), which Task 2
depends on.

- [ ] **Step 1: Create the fetch script**

Create `scripts/fetch-xray.sh`:

```sh
#!/bin/sh
# Downloads xray-core for every platform the agent targets, verifies its
# SHA2-256 against the release's own .dgst file, and extracts just the
# binary (not geoip.dat/geosite.dat/wintun.dll - internal/checks.VLESSChecker
# doesn't use xray's geo-routing) into internal/checks/embedded/<goos>_<goarch>/.
# Run once before scripts/build-agent.sh's build loop - see
# internal/checks/xray_*.go's //go:embed directives for where these end up
# mattering.
#
# Nothing here runs on a deployed node - see
# docs/superpowers/specs/2026-08-12-vless-speedtest-check-design.md for why
# that's a hard requirement, not a convenience.
#
# Usage:
#   sh scripts/fetch-xray.sh              # fetch all 6 platforms
#   sh scripts/fetch-xray.sh linux amd64  # fetch just one (used by Dockerfile)
set -eu

cd "$(dirname "$0")/.."

XRAY_VERSION="v26.3.27"
BASE_URL="https://github.com/XTLS/Xray-core/releases/download/${XRAY_VERSION}"

checksum() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

fetch_one() {
  goos="$1"; goarch="$2"; asset="$3"; binname="$4"
  dir="internal/checks/embedded/${goos}_${goarch}"
  mkdir -p "$dir"
  dest="$dir/$binname"

  if [ -f "$dest" ]; then
    echo "already have $dest, skipping"
    return
  fi

  tmpdir=$(mktemp -d)
  echo "fetching $asset ($goos/$goarch)..."
  curl -sL -o "$tmpdir/$asset" "$BASE_URL/$asset"
  curl -sL -o "$tmpdir/$asset.dgst" "$BASE_URL/$asset.dgst"

  want=$(grep '^SHA2-256=' "$tmpdir/$asset.dgst" | cut -d= -f2 | tr -d ' ')
  got=$(checksum "$tmpdir/$asset")
  if [ "$want" != "$got" ]; then
    echo "checksum mismatch for $asset: want $want, got $got" >&2
    rm -rf "$tmpdir"
    exit 1
  fi

  unzip -p "$tmpdir/$asset" "$binname" > "$dest"
  chmod +x "$dest"
  rm -rf "$tmpdir"
  echo "-> $dest"
}

target_goos="${1:-}"
target_goarch="${2:-}"

fetch_matching() {
  goos="$1"; goarch="$2"; asset="$3"; binname="$4"
  if [ -n "$target_goos" ]; then
    [ "$goos" = "$target_goos" ] && [ "$goarch" = "$target_goarch" ] || return 0
  fi
  fetch_one "$goos" "$goarch" "$asset" "$binname"
}

fetch_matching windows amd64 Xray-windows-64.zip      xray.exe
fetch_matching windows 386   Xray-windows-32.zip      xray.exe
fetch_matching linux   amd64 Xray-linux-64.zip         xray
fetch_matching linux   arm64 Xray-linux-arm64-v8a.zip  xray
fetch_matching darwin  amd64 Xray-macos-64.zip         xray
fetch_matching darwin  arm64 Xray-macos-arm64-v8a.zip  xray

echo "done: internal/checks/embedded/"
```

- [ ] **Step 2: Add the embedded-binaries directory to `.gitignore`**

Open `.gitignore`, add a new line:
```
/internal/checks/embedded/
```

- [ ] **Step 3: Run the fetch script**

Run: `sh scripts/fetch-xray.sh`

Expected: fetches 6 files (one per platform), each preceded by a checksum
verification. This downloads roughly 120 MB of zips and extracts ~210 MB
of binaries total - expect this to take a few minutes depending on
connection speed. Verify at the end:

Run: `find internal/checks/embedded -type f`
Expected: exactly 6 files:
```
internal/checks/embedded/windows_amd64/xray.exe
internal/checks/embedded/windows_386/xray.exe
internal/checks/embedded/linux_amd64/xray
internal/checks/embedded/linux_arm64/xray
internal/checks/embedded/darwin_amd64/xray
internal/checks/embedded/darwin_arm64/xray
```

- [ ] **Step 4: Create the six platform-gated embed files**

Create `internal/checks/xray_windows_amd64.go`:
```go
//go:build windows && amd64

package checks

import _ "embed"

//go:embed embedded/windows_amd64/xray.exe
var embeddedXrayBinary []byte

const embeddedXrayFilename = "xray.exe"
```

Create `internal/checks/xray_windows_386.go`:
```go
//go:build windows && 386

package checks

import _ "embed"

//go:embed embedded/windows_386/xray.exe
var embeddedXrayBinary []byte

const embeddedXrayFilename = "xray.exe"
```

Create `internal/checks/xray_linux_amd64.go`:
```go
//go:build linux && amd64

package checks

import _ "embed"

//go:embed embedded/linux_amd64/xray
var embeddedXrayBinary []byte

const embeddedXrayFilename = "xray"
```

Create `internal/checks/xray_linux_arm64.go`:
```go
//go:build linux && arm64

package checks

import _ "embed"

//go:embed embedded/linux_arm64/xray
var embeddedXrayBinary []byte

const embeddedXrayFilename = "xray"
```

Create `internal/checks/xray_darwin_amd64.go`:
```go
//go:build darwin && amd64

package checks

import _ "embed"

//go:embed embedded/darwin_amd64/xray
var embeddedXrayBinary []byte

const embeddedXrayFilename = "xray"
```

Create `internal/checks/xray_darwin_arm64.go`:
```go
//go:build darwin && arm64

package checks

import _ "embed"

//go:embed embedded/darwin_arm64/xray
var embeddedXrayBinary []byte

const embeddedXrayFilename = "xray"
```

- [ ] **Step 5: Verify the build picks up the embed correctly**

Run: `go build ./...`
Expected: builds cleanly (on this Windows/amd64 machine, only
`xray_windows_amd64.go`'s embed directive is active - build tags exclude
the other five files entirely, so their binaries don't need to exist for
this to succeed). If this fails with `pattern embedded/windows_amd64/xray.exe:
no matching files found`, Step 3 didn't complete - re-run
`sh scripts/fetch-xray.sh` before continuing.

- [ ] **Step 6: Commit**

```bash
git add scripts/fetch-xray.sh .gitignore internal/checks/xray_windows_amd64.go internal/checks/xray_windows_386.go internal/checks/xray_linux_amd64.go internal/checks/xray_linux_arm64.go internal/checks/xray_darwin_amd64.go internal/checks/xray_darwin_arm64.go
git commit -m "Add xray-core fetch script and per-platform go:embed scaffolding

scripts/fetch-xray.sh pulls the pinned v26.3.27 release for each of the
agent's 6 target platforms, checksum-verifies it, and extracts just the
binary into internal/checks/embedded/ (gitignored - large, mechanically
fetched, not source). Six new build-tag-gated files each embed exactly
their own platform's binary into embeddedXrayBinary, so cross-compiling
for one platform never needs the other five present.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
```

---

## Task 2: `VLESSChecker`

**Files:**
- Create: `internal/checks/vless.go`
- Create: `internal/checks/vless_test.go`
- Modify: `internal/checks/checks.go` (registry)
- Modify: `go.mod`, `go.sum` (new dependency)

- [ ] **Step 1: Add the SOCKS5 dial dependency**

Run: `go get golang.org/x/net/proxy`
Expected: `go.mod` gains a `require golang.org/x/net vX.Y.Z` line (or
updates an existing indirect one to direct), `go.sum` gains matching
entries.

- [ ] **Step 2: Write the failing tests**

Create `internal/checks/vless_test.go`:

```go
package checks

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestPatchInboundAddsSocksWhenNoneExists(t *testing.T) {
	input := json.RawMessage(`{"outbounds":[{"protocol":"vless"}]}`)
	patched, err := patchInbound(input, 12345)
	if err != nil {
		t.Fatalf("patchInbound() error = %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(patched, &doc); err != nil {
		t.Fatalf("unmarshal patched config: %v", err)
	}

	inbounds, ok := doc["inbounds"].([]any)
	if !ok || len(inbounds) != 1 {
		t.Fatalf("inbounds = %v, want exactly one entry", doc["inbounds"])
	}
	entry := inbounds[0].(map[string]any)
	if entry["protocol"] != "socks" {
		t.Errorf("inbound protocol = %v, want socks", entry["protocol"])
	}
	if entry["listen"] != "127.0.0.1" {
		t.Errorf("inbound listen = %v, want 127.0.0.1", entry["listen"])
	}
	if int(entry["port"].(float64)) != 12345 {
		t.Errorf("inbound port = %v, want 12345", entry["port"])
	}

	outbounds, ok := doc["outbounds"].([]any)
	if !ok || len(outbounds) != 1 {
		t.Fatalf("outbounds = %v, want the original single entry preserved", doc["outbounds"])
	}
}

func TestPatchInboundReplacesExistingInbounds(t *testing.T) {
	input := json.RawMessage(`{"inbounds":[{"protocol":"http","port":8080}],"outbounds":[{"protocol":"vless"}]}`)
	patched, err := patchInbound(input, 55555)
	if err != nil {
		t.Fatalf("patchInbound() error = %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(patched, &doc); err != nil {
		t.Fatalf("unmarshal patched config: %v", err)
	}

	inbounds := doc["inbounds"].([]any)
	if len(inbounds) != 1 {
		t.Fatalf("inbounds = %d entries, want exactly 1 (the caller's http inbound must be discarded)", len(inbounds))
	}
	entry := inbounds[0].(map[string]any)
	if entry["protocol"] != "socks" {
		t.Errorf("inbound protocol = %v, want socks (the caller's original http inbound must not survive)", entry["protocol"])
	}
	if int(entry["port"].(float64)) != 55555 {
		t.Errorf("inbound port = %v, want 55555", entry["port"])
	}
}

func TestPatchInboundRejectsInvalidJSON(t *testing.T) {
	_, err := patchInbound(json.RawMessage(`not json`), 1234)
	if err == nil {
		t.Fatal("patchInbound() error = nil, want an error for invalid JSON")
	}
}

func TestFreePortReturnsUsablePorts(t *testing.T) {
	a, err := freePort()
	if err != nil {
		t.Fatalf("freePort() error = %v", err)
	}
	if a <= 0 {
		t.Errorf("freePort() = %d, want a positive port number", a)
	}
	b, err := freePort()
	if err != nil {
		t.Fatalf("freePort() error = %v", err)
	}
	if b <= 0 {
		t.Errorf("freePort() = %d, want a positive port number", b)
	}
}

func TestClassifyXrayStartupError(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		want   string
	}{
		{"empty", "", "xray failed to start (no output)"},
		{"single line", "config: invalid protocol", "xray config error: config: invalid protocol"},
		{"multi line takes last", "line one\nline two\nreal error here", "xray config error: real error here"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyXrayStartupError(tc.stderr); got != tc.want {
				t.Errorf("classifyXrayStartupError(%q) = %q, want %q", tc.stderr, got, tc.want)
			}
		})
	}
}

func TestEnsureXrayBinaryInWritesAndReusesCache(t *testing.T) {
	// Swap the package-level embedded bytes for a small fake payload so
	// this test doesn't depend on (or exercise) the real ~35MB binary -
	// ensureXrayBinaryIn only cares about the byte slice's identity/length,
	// never its actual content.
	original := embeddedXrayBinary
	embeddedXrayBinary = []byte("fake-xray-binary-for-testing")
	defer func() { embeddedXrayBinary = original }()

	dir := t.TempDir()

	path1, err := ensureXrayBinaryIn(dir)
	if err != nil {
		t.Fatalf("ensureXrayBinaryIn() error = %v", err)
	}
	info1, err := os.Stat(path1)
	if err != nil {
		t.Fatalf("stat %s: %v", path1, err)
	}
	if info1.Size() != int64(len(embeddedXrayBinary)) {
		t.Errorf("written file size = %d, want %d", info1.Size(), len(embeddedXrayBinary))
	}

	// Second call must reuse the same file, not rewrite it - if it
	// rewrites, the mtime would move forward; sleep briefly so a
	// wrongly-rewritten file would show a detectably later mtime.
	time.Sleep(20 * time.Millisecond)
	path2, err := ensureXrayBinaryIn(dir)
	if err != nil {
		t.Fatalf("ensureXrayBinaryIn() second call error = %v", err)
	}
	if path2 != path1 {
		t.Errorf("second call path = %q, want the same path %q", path2, path1)
	}
	info2, err := os.Stat(path2)
	if err != nil {
		t.Fatalf("stat %s: %v", path2, err)
	}
	if !info2.ModTime().Equal(info1.ModTime()) {
		t.Errorf("mtime changed between calls (%v -> %v) - ensureXrayBinaryIn rewrote a file it should have reused", info1.ModTime(), info2.ModTime())
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/checks/... -run 'TestPatchInbound|TestFreePort|TestClassifyXrayStartupError|TestEnsureXrayBinaryIn'`
Expected: FAIL to build - `undefined: patchInbound`, `undefined: freePort`,
`undefined: classifyXrayStartupError`, `undefined: ensureXrayBinaryIn`.

- [ ] **Step 4: Create `internal/checks/vless.go`**

```go
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
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
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
		msg := classifyXrayStartupError(stderr.String())
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
// routing, ...) passes through with its original content unchanged.
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
```

- [ ] **Step 5: Register `"vless"` in the checker registry**

Open `internal/checks/checks.go`, edit the `registry` map:

```go
var registry = map[string]Checker{
	"ping":    PingChecker{},
	"tcp":     TCPChecker{},
	"http":    HTTPChecker{},
	"dns":     DNSChecker{},
	"tls":     TLSChecker{},
	"upgrade": UpgradeChecker{},
	"vless":   VLESSChecker{},
}
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go build ./... && go vet ./... && go test ./internal/checks/... -v`
Expected: BUILD OK, VET OK, all new tests PASS
(`TestPatchInboundAddsSocksWhenNoneExists`,
`TestPatchInboundReplacesExistingInbounds`,
`TestPatchInboundRejectsInvalidJSON`, `TestFreePortReturnsUsablePorts`,
`TestClassifyXrayStartupError` + 3 subtests,
`TestEnsureXrayBinaryInWritesAndReusesCache`), and every pre-existing test
in the package still passes (no regressions).

- [ ] **Step 7: Commit**

```bash
git add go.mod go.sum internal/checks/vless.go internal/checks/vless_test.go internal/checks/checks.go
git commit -m "Add vless check type: VLESS tunnel + download speed test

Patches the caller's xray-core config to a private local SOCKS5 inbound
(fresh port per invocation - a node can run several checks concurrently),
runs xray as a subprocess, speed-tests through it against a fixed
Cloudflare endpoint (never caller-supplied, to avoid turning nodes into an
open download relay), reports Mbps. No unit test exercises a real VLESS
server end-to-end - that needs live infrastructure this repo doesn't
stand up for tests, verified manually once wired into the bot.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
```

---

## Task 3: Backend also embeds xray-core (Docker)

**Files:**
- Modify: `Dockerfile`

Now that `internal/checks` unconditionally has six `//go:build`-gated
files, `cmd/server`'s own build (always `linux/amd64` in production, per
this Dockerfile) needs `internal/checks/embedded/linux_amd64/xray` to
exist inside the build container - it isn't in git (Task 1's
`.gitignore` entry), so the Docker build has to fetch it itself, not rely
on it being present on the host running `docker build`.

- [ ] **Step 1: Update the Dockerfile**

Read `Dockerfile` first to confirm its current exact contents, then
replace the build stage with:

```dockerfile
# Backend server image. Migrations are baked in and applied automatically
# on startup (internal/store.RunMigrations) - no separate migration step
# needed at deploy time.
FROM golang:1.26-alpine AS build
WORKDIR /src
RUN apk add --no-cache curl unzip

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/server ./cmd/server
COPY internal ./internal
COPY scripts/fetch-xray.sh ./scripts/fetch-xray.sh
RUN sh scripts/fetch-xray.sh linux amd64
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/pingachock-server ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/pingachock-server /usr/local/bin/pingachock-server
COPY migrations /migrations
ENV MIGRATIONS_DIR=/migrations
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/pingachock-server"]
```

The only changes from the original: `RUN apk add --no-cache curl unzip`
(fetch-xray.sh needs both), `COPY scripts/fetch-xray.sh ...` +
`RUN sh scripts/fetch-xray.sh linux amd64` inserted between `COPY internal
./internal` and the `go build` line.

- [ ] **Step 2: Verify the image builds**

Run: `docker build -t pingachock3-backend-test .`
Expected: builds successfully through both stages - the `fetch-xray.sh`
step downloads and verifies the linux/amd64 binary inside the container
(a few minutes), then `go build` succeeds because
`internal/checks/embedded/linux_amd64/xray` now exists in the build
context. If this fails at the `go build` step with the same "no matching
files" embed error from Task 1, double check the `COPY`/`RUN` ordering
above - `fetch-xray.sh` must run *after* `COPY internal ./internal` (it
writes into that copied tree) and *before* `go build`.

- [ ] **Step 3: Commit**

```bash
git add Dockerfile
git commit -m "Dockerfile: fetch linux/amd64 xray-core inside the build stage

internal/checks now unconditionally has build-tag-gated go:embed files;
cmd/server's own linux/amd64 build needs its matching binary present,
which isn't in git (see Task 1) - fetched fresh inside the container via
the same scripts/fetch-xray.sh the agent build uses.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
```

---

## Task 4: Bot API client (`checkVlessSpeed`)

**Files:**
- Modify: `bot/src/pingachock-client.ts`
- Create: `bot/src/vless-speedtest.test.ts`

- [ ] **Step 1: Write the failing test**

Create `bot/src/vless-speedtest.test.ts`:

```ts
import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

// Pure logic - see classification.test.ts's comment on why db.ts still
// needs pointing at a throwaway dir before import.
const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'pingachock-bot-test-'));
process.env.DB_PATH = path.join(tmpDir, 'users.db');
process.env.SETTINGS_DB_PATH = path.join(tmpDir, 'settings.db');

const { mapVlessSpeedTestResult } = require('./pingachock-client') as typeof import('./pingachock-client');

test('successful run extracts mbps from the matching node_id run', () => {
  const check = {
    runs: [
      { node_id: 'other-node', result: { success: true, raw: '{"mbps":50}' } },
      { node_id: 'my-node', result: { success: true, raw: '{"mbps":123.45,"bytes_downloaded":10000000,"duration_ms":647}' } }
    ]
  };
  const got = mapVlessSpeedTestResult(check, 'my-node');
  assert.equal(got.success, true);
  assert.equal(got.mbps, 123.45);
});

test('failed run carries a translated error message, not the raw token', () => {
  const check = {
    runs: [{ node_id: 'my-node', result: { success: false, error_message: 'timeout' } }]
  };
  const got = mapVlessSpeedTestResult(check, 'my-node');
  assert.equal(got.success, false);
  assert.equal(got.errorMessage, 'таймаут');
});

test('no matching run for this node_id is treated as no response', () => {
  const check = { runs: [{ node_id: 'other-node', result: { success: true, raw: '{"mbps":50}' } }] };
  const got = mapVlessSpeedTestResult(check, 'my-node');
  assert.equal(got.success, false);
  assert.equal(got.errorMessage, 'нет ответа от узла');
});

test('missing raw on an otherwise-successful result still reports success with no mbps', () => {
  const check = { runs: [{ node_id: 'my-node', result: { success: true } }] };
  const got = mapVlessSpeedTestResult(check, 'my-node');
  assert.equal(got.success, true);
  assert.equal(got.mbps, undefined);
});
```

- [ ] **Step 2: Run the test to verify it fails**

Run (from `bot/`): `npx tsx --test src/vless-speedtest.test.ts`
Expected: FAIL - `mapVlessSpeedTestResult is not a function`.

- [ ] **Step 3: Give `pollCheckUntilDone` a per-call timeout override**

Read `bot/src/pingachock-client.ts`'s current `pollCheckUntilDone`
function first, then replace it:

```ts
async function pollCheckUntilDone(checkId: string, timeoutMs: number = NODE_POLL_TIMEOUT_MS): Promise<any> {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const check = await fetchWithAuth(`/api/v1/checks/${checkId}?expand=runs`, 'GET', undefined, 'api');
    const status = (check as any)?.status;
    if (status !== 'pending' && status !== 'running') return check;
    if (Date.now() > deadline) return check; // give up, report whatever we have
    await sleep(NODE_POLL_INTERVAL_MS);
  }
}
```

(The only change from the current version is the new `timeoutMs`
parameter with `NODE_POLL_TIMEOUT_MS` as its default - every existing
caller keeps working unchanged since none of them pass a second
argument.) This exists because a VLESS check can legitimately take up to
`vlessOverallTimeout` (30s) on the Go side *before* the HTTP response even
starts (xray startup + up to a 15s speed test), on top of normal polling
time - the default 60s `NODE_POLL_TIMEOUT_MS` (right for ping/tcp/tls,
which complete in seconds) isn't enough headroom.

- [ ] **Step 4: Add `mapVlessSpeedTestResult` and `checkVlessSpeed` to `bot/src/pingachock-client.ts`**

Add at the bottom of the file, after the `scanUpgrade` export added by the
HTTP 101 check feature:

```ts
export type VlessSpeedTestResult = { success: boolean; mbps?: number; errorMessage?: string };

function parseVlessRaw(raw: unknown): { mbps: number | null } {
  if (!raw) return { mbps: null };
  try {
    const parsed = typeof raw === 'string' ? JSON.parse(raw) : raw;
    return { mbps: typeof parsed?.mbps === 'number' ? parsed.mbps : null };
  } catch {
    return { mbps: null };
  }
}

// mapVlessSpeedTestResult is split out from checkVlessSpeed so the
// response-mapping itself is unit-testable without a live backend or a
// real VLESS server - mirrors mapUpgradeScanResults's role for scanUpgrade.
export function mapVlessSpeedTestResult(check: any, nodeId: string): VlessSpeedTestResult {
  const run = check?.runs?.find((r: any) => r.node_id === nodeId);
  const result = run?.result;
  if (!result) {
    return { success: false, errorMessage: 'нет ответа от узла' };
  }
  if (!result.success) {
    return { success: false, errorMessage: translateCheckError(result.error_message) ?? 'ошибка' };
  }
  const { mbps } = parseVlessRaw(result.raw);
  return { success: true, mbps: mbps ?? undefined };
}

// checkVlessSpeed: dispatches a "vless" check to one node - always a real
// node, never "server" (there is no server-side equivalent for this
// check, it only means something from a node's own network vantage
// point). config is the full xray-core config.json the caller already
// validated as parseable JSON. 90000ms: comfortably above the Go side's
// own 30s vlessOverallTimeout plus poll-interval slack, while staying
// under Telegraf's handlerTimeout: 120_000 configured in
// bot/src/index.ts - see NODE_POLL_TIMEOUT_MS's own doc comment for why
// that headroom matters (a real past incident, not a hypothetical one).
// See docs/superpowers/specs/2026-08-12-vless-speedtest-check-design.md.
export async function checkVlessSpeed(config: unknown, routerName: string): Promise<VlessSpeedTestResult> {
  const { id: nodeId } = await resolveNodeId(routerName);
  const created = (await fetchWithAuth(
    '/api/v1/checks',
    'POST',
    { type: 'vless', targets: ['vless-speedtest'], params: { config }, node_selector: { node_ids: [nodeId] } },
    'api'
  )) as any;
  const checkId = created.batch_id ? created.checks[0].id : created.id;
  const check = await pollCheckUntilDone(checkId, 90000);
  return mapVlessSpeedTestResult(check, nodeId);
}
```

- [ ] **Step 5: Run the test to verify it passes**

Run (from `bot/`): `npx tsx --test src/vless-speedtest.test.ts`
Expected: PASS (4 tests, 0 failures).

- [ ] **Step 6: Type-check and regression-test the whole bot**

Run (from `bot/`): `npx tsc --noEmit && npx tsx --test src/*.test.ts`
Expected: clean type-check; every test passes except the live-backend-only
`pingachock-client.test.ts` (pre-existing, expected - see the HTTP 101
check feature's own Task 4 for this same caveat).

- [ ] **Step 7: Commit**

```bash
git add bot/src/pingachock-client.ts bot/src/vless-speedtest.test.ts
git commit -m "Bot: add checkVlessSpeed client, give pollCheckUntilDone a timeout override

VLESS checks can legitimately take up to 30s on the Go side before the
HTTP response even starts (xray startup + up to a 15s speed test) - the
existing 60s NODE_POLL_TIMEOUT_MS default was designed around ping/tcp/tls
checks that complete in seconds, not this. pollCheckUntilDone now takes an
optional per-call override instead of duplicating its poll loop.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
```

---

## Task 5: Bot UI ("Дополнительные проверки" → "VLESS Speedtest")

**Files:**
- Modify: `bot/src/index.ts`

Mirrors the router-toggle-then-type shape used elsewhere in this bot
(`getPingRouterOption`/`pingKeyboard`), with its own independent session
state so it can't cross-contaminate ping's. No `ALL` node option - same
reasoning as the (not-yet-built) TLS Handshake Check design: this only
makes sense against one chosen node at a time. No isolated unit test (pure
Telegraf wiring around the already-tested `checkVlessSpeed`) - verified
via `tsc --noEmit` + manual runthrough, same as every other menu flow in
this file.

- [ ] **Step 1: Add session state**

In `bot/src/index.ts`, find the `MySession` type. Add these fields near
the other feature-specific blocks (after `awaitingUpgradeScanTargets` if
that's the last one added, from the HTTP 101 check feature):

```ts
  awaitingVlessConfig?: boolean;
  vlessRouterIndex?: number;
  vlessRouters?: Router[];
```

- [ ] **Step 2: Add a VLESS-specific router option resolver and keyboard**

Add near `getPingRouterOption`/`pingKeyboard` (reuses the same
`pingRouterLabels` constant already defined there - `Auto`/`ALL`, though
`ALL` is never shown for this feature, see below):

```ts
function getVlessRouterOption(session: MySession): { label: string; value: string } {
  const routers = session.vlessRouters ?? [];
  const options: Array<{ label: string; value: string }> = [
    { label: pingRouterLabels.auto, value: 'auto' },
    ...routers.map((r) => ({ label: r.name, value: r.name }))
  ];
  const index = session.vlessRouterIndex ?? 0;
  return options[Math.max(0, Math.min(index, options.length - 1))];
}

function vlessKeyboard(session: MySession) {
  const routerOpt = getVlessRouterOption(session);
  return Markup.inlineKeyboard([
    [Markup.button.callback(routerOpt.label, 'extra:vless_toggle_router')],
    [Markup.button.callback('◀️ Назад', 'menu:root')]
  ]);
}
```

(Deliberately no `__all__` entry in `options` here, unlike
`getPingRouterOption` - see the plan header's Scope note.)

- [ ] **Step 3: Add the fourth "Дополнительные проверки" button**

Find `extraChecksKeyboard()` (added by the HTTP 101 check feature) and add
a new row:

```ts
function extraChecksKeyboard() {
  return Markup.inlineKeyboard([
    [Markup.button.callback('HTTP 101 check (Websocket)', 'extra:http101')],
    [Markup.button.callback('VLESS Speedtest', 'extra:vlessspeedtest')],
    [Markup.button.callback('◀️ Назад', 'menu:root')]
  ]);
}
```

- [ ] **Step 4: Add the action handlers**

Find the `extra:http101` handler (added by the HTTP 101 check feature) and
add these three new handlers right after it:

```ts
bot.action('extra:vlessspeedtest', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  await ctx.answerCbQuery();

  ctx.session.vlessRouterIndex = ctx.session.vlessRouterIndex ?? 0;
  try {
    const allRouters = await apiClient.listRouters();
    ctx.session.vlessRouters = allRouters.filter((r) => r.status === 'online');
    const optionsLen = 1 + (ctx.session.vlessRouters?.length ?? 0);
    if (optionsLen > 0 && (ctx.session.vlessRouterIndex ?? 0) >= optionsLen) {
      ctx.session.vlessRouterIndex = 0;
    }
  } catch (err) {
    const errMsg = err instanceof Error ? err.message : String(err);
    await safeEditOrReply(ctx, `Ошибка:\n${errMsg}`, extraChecksKeyboard());
    return;
  }

  ctx.session.awaitingVlessConfig = true;
  await safeEditOrReply(
    ctx,
    'Выбери узел (кнопка выше) и пришли конфиг xray-core целиком (JSON) одним сообщением.',
    vlessKeyboard(ctx.session)
  );
});

bot.action('extra:vless_toggle_router', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  await ctx.answerCbQuery();
  const optionsLen = 1 + (ctx.session.vlessRouters?.length ?? 0);
  ctx.session.vlessRouterIndex = ((ctx.session.vlessRouterIndex ?? 0) + 1) % Math.max(1, optionsLen);
  await safeEditOrIgnore(
    ctx,
    'Выбери узел (кнопка выше) и пришли конфиг xray-core целиком (JSON) одним сообщением.',
    vlessKeyboard(ctx.session)
  );
});
```

Also update the existing `menu:extra` handler (added by the HTTP 101
check feature) to reset the new flag alongside the existing one:

```ts
bot.action('menu:extra', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  await ctx.answerCbQuery();
  ctx.session.awaitingUpgradeScanTargets = false;
  ctx.session.awaitingVlessConfig = false;
  await safeEditOrReply(ctx, 'Дополнительные проверки:', extraChecksKeyboard());
});
```

And `menu:root`'s existing reset line, adding the new flag:

```ts
bot.action('menu:root', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  ctx.session.awaitingPingInput = false;
  ctx.session.awaitingUpgradeScanTargets = false;
  ctx.session.awaitingVlessConfig = false;
  await ctx.answerCbQuery();
  await safeEditOrReply(ctx, await renderMainMenuText(), mainMenuKeyboard());
});
```

- [ ] **Step 5: Add the text-input handler**

Find the `bot.on('text', async (ctx, next) => {` block. Locate the HTTP
101 check's own input block (search for `awaitingUpgradeScanTargets &&
(await isAuthorizedUser(ctx))`) and add this new block immediately before
it:

```ts
  // Дополнительные проверки: VLESS Speedtest — ждём конфиг
  if (ctx.session.awaitingVlessConfig && (await isAuthorizedUser(ctx))) {
    const input = ctx.message.text.trim();
    let config: unknown;
    try {
      config = JSON.parse(input);
    } catch (err) {
      const errMsg = err instanceof Error ? err.message : String(err);
      await ctx.reply(`Конфиг не распарсился как JSON: ${errMsg}`, vlessKeyboard(ctx.session));
      return;
    }

    const routerOpt = getVlessRouterOption(ctx.session);
    ctx.session.awaitingVlessConfig = false;

    const waitMsg = await ctx.reply('Поднимаю VLESS-туннель и гоняю speedtest, это может занять до ~40 секунд...');
    try {
      const result = await apiClient.checkVlessSpeed(config, routerOpt.value);
      const reportText = result.success
        ? `VLESS Speedtest\nВремя проверки: ${formatHumanDate(new Date())}\nУзел: ${routerOpt.label}\n\n✅ ${result.mbps != null ? result.mbps.toFixed(1) + ' Mbps' : 'туннель поднялся, скорость не измерена'}`
        : `VLESS Speedtest\nВремя проверки: ${formatHumanDate(new Date())}\nУзел: ${routerOpt.label}\n\n❌ ошибка: ${result.errorMessage ?? 'неизвестная ошибка'}`;
      await ctx.reply(reportText, extraChecksKeyboard());
    } catch (err) {
      const errMsg = err instanceof Error ? err.message : String(err);
      await ctx.reply(`Ошибка:\n${errMsg}`, extraChecksKeyboard());
    } finally {
      try {
        await ctx.telegram.deleteMessage(ctx.chat!.id, waitMsg.message_id);
      } catch {
        // best-effort - not fatal if Telegram won't let us delete it
      }
    }
    return;
  }

```

- [ ] **Step 6: Type-check**

Run (from `bot/`): `npx tsc --noEmit`
Expected: no output, exit code 0.

- [ ] **Step 7: Production build**

Run (from `bot/`): `npm run build`
Expected: `tsc -p tsconfig.json` completes with no errors.

- [ ] **Step 8: Run the full bot pure-logic test suite (regression check)**

Run (from `bot/`): `npx tsx --test src/*.test.ts`
Expected: every test passes except the live-backend-only
`pingachock-client.test.ts`.

- [ ] **Step 9: Commit**

```bash
git add bot/src/index.ts
git commit -m "Bot: wire up 'Дополнительные проверки' -> VLESS Speedtest menu

Fourth entry in the extra-checks submenu. Own independent router-toggle
session state (vlessRouterIndex/vlessRouters), no ALL option - same
reasoning as the TLS Handshake Check design (one chosen node at a time).
Config is pasted as a text message (validated as parseable JSON before
dispatch, not left to the backend to reject); a 'poll takes up to ~40s'
notice is shown and cleaned up afterward since this is meaningfully slower
than every other check in this menu.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
```

---

## Task 6: Final verification

**Files:** none (verification only)

- [ ] **Step 1: Full Go verification**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: BUILD OK, VET OK, every package's tests pass.

- [ ] **Step 2: Full bot verification**

Run (from `bot/`): `npx tsc --noEmit && npm run build && npx tsx --test src/*.test.ts`
Expected: clean type-check, clean build, all tests pass except the
live-backend-only `pingachock-client.test.ts`.

- [ ] **Step 3: Confirm the full agent build still works end-to-end**

Run: `sh scripts/build-agent.sh`
Expected: `fetch-xray.sh` runs first (should report "already have ...,
skipping" for all 6 files from Task 1's Step 3), then all 6 agent
binaries build successfully in `bin/`. Spot-check the size increase:

Run: `ls -la bin/pingachock-agent-windows-amd64.exe`
Expected: roughly 45-55 MB (was ~10 MB before this feature - see the
design doc's own size disclosure).

- [ ] **Step 4: Manual smoke test, if a real VLESS server + reachable backend are available**

This needs live infrastructure this repo doesn't stand up for automated
tests: a running backend, a node online and polling it, and a real VLESS
server to point a config at. If available, exercise the full flow through
the bot (➕ Дополнительные проверки → VLESS Speedtest) with a real config
and confirm a plausible Mbps comes back. If nothing is reachable in this
environment, skip this step and say so explicitly when reporting
completion - do not fabricate a result.

- [ ] **Step 5: Update the design spec's status**

In `docs/superpowers/specs/2026-08-12-vless-speedtest-check-design.md`,
change:
```
Status: APPROVED, ready for implementation planning.
```
to:
```
Status: DONE. Implemented per docs/superpowers/plans/2026-08-12-vless-speedtest-check.md.
```

- [ ] **Step 6: Commit and push**

```bash
git add docs/superpowers/specs/2026-08-12-vless-speedtest-check-design.md
git commit -m "Mark VLESS speedtest check design DONE

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
git push origin main
```
