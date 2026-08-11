# HTTP 101 Upgrade Check Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a server-only "does this host answer HTTP 101 Switching Protocols to a plaintext Connection:Upgrade request on port 443" check, reachable via a new public API endpoint and, on top of it, a bot menu.

**Architecture:** A new `checks.Checker` (`internal/checks/upgrade.go`, plain TCP - not TLS-wrapped) plumbed into a new synchronous handler (`internal/api/public/serverupgradescan.go`, modeled on the existing `ServerPing` handler: no DB, no check_runs, fully self-contained per request) at `POST /api/v1/server-upgrade-scan`. The bot (`bot/src/pingachock-client.ts`, `bot/src/index.ts`) is one consumer of that endpoint via a new "➕ Дополнительные проверки" menu.

**Tech Stack:** Go (stdlib `net`, no new dependencies), TypeScript/Telegraf (existing bot).

**Spec:** `docs/superpowers/specs/2026-08-09-http-101-upgrade-check-design.md`

---

## Task 1: `UpgradeChecker` (Go)

**Files:**
- Create: `internal/checks/upgrade.go`
- Create: `internal/checks/upgrade_test.go`
- Modify: `internal/checks/checks.go:38-44` (registry)

- [ ] **Step 1: Write the failing tests**

Create `internal/checks/upgrade_test.go`:

```go
package checks

import (
	"context"
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMatchSwitchingProtocols(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"real 101", "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n\r\n", true},
		{"400", "HTTP/1.1 400 Bad Request\r\n\r\n", false},
		{"200", "HTTP/1.1 200 OK\r\n\r\n", false},
		{"empty", "", false},
		{"garbage", "not even http", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchSwitchingProtocols([]byte(tc.body)); got != tc.want {
				t.Errorf("matchSwitchingProtocols(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

func TestBuildUpgradeRequestWebsocket(t *testing.T) {
	req := string(buildUpgradeRequest("example.com", "websocket"))
	for _, want := range []string{
		"GET / HTTP/1.1\r\n",
		"Host: example.com\r\n",
		"Connection: Upgrade\r\n",
		"Upgrade: websocket\r\n",
		"Sec-WebSocket-Version: 13\r\n",
		"Sec-WebSocket-Key: ",
	} {
		if !strings.Contains(req, want) {
			t.Errorf("request missing %q:\n%s", want, req)
		}
	}
	if !strings.HasSuffix(req, "\r\n\r\n") {
		t.Errorf("request must end with a blank line, got:\n%q", req)
	}
}

// startRawServer runs handle for every accepted connection on a fresh
// 127.0.0.1 port, until closeFn is called.
func startRawServer(t *testing.T, handle func(net.Conn)) (port int, closeFn func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handle(conn)
		}
	}()
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ = strconv.Atoi(portStr)
	return port, func() { ln.Close() }
}

func TestUpgradeCheckerMatchesRealSwitchingProtocols(t *testing.T) {
	port, closeFn := startRawServer(t, func(conn net.Conn) {
		defer conn.Close()
		buf := make([]byte, 4096)
		_, _ = conn.Read(buf) // drain the request
		_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"))
	})
	defer closeFn()

	params, _ := json.Marshal(map[string]any{"port": port})
	res := UpgradeChecker{}.Run(context.Background(), NetConfig{}, "127.0.0.1", params)

	if !res.Success {
		errMsg := "nil"
		if res.ErrorMessage != nil {
			errMsg = *res.ErrorMessage
		}
		t.Fatalf("Success = false, want true (error: %s)", errMsg)
	}
}

func TestUpgradeCheckerDoesNotMatchNormalResponse(t *testing.T) {
	port, closeFn := startRawServer(t, func(conn net.Conn) {
		defer conn.Close()
		buf := make([]byte, 4096)
		_, _ = conn.Read(buf)
		_, _ = conn.Write([]byte("HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n"))
	})
	defer closeFn()

	params, _ := json.Marshal(map[string]any{"port": port})
	res := UpgradeChecker{}.Run(context.Background(), NetConfig{}, "127.0.0.1", params)

	if res.Success {
		t.Fatal("Success = true, want false - server answered 400, not 101")
	}
	if res.ErrorMessage != nil {
		t.Errorf("ErrorMessage = %v, want nil - a normal non-101 response isn't a transport error", *res.ErrorMessage)
	}
}

func TestUpgradeCheckerConnectionRefused(t *testing.T) {
	// Port 1 is reserved (tcpmux) and nothing should ever be listening on
	// it - dial should fail fast and consistently across environments.
	params, _ := json.Marshal(map[string]any{"port": 1, "timeout_ms": 1000})
	res := UpgradeChecker{}.Run(context.Background(), NetConfig{}, "127.0.0.1", params)

	if res.Success {
		t.Fatal("Success = true, want false - nothing is listening")
	}
	if res.ErrorMessage == nil {
		t.Fatal("ErrorMessage is nil, want a connection error")
	}
}

func TestUpgradeCheckerTimeout(t *testing.T) {
	port, closeFn := startRawServer(t, func(conn net.Conn) {
		defer conn.Close()
		time.Sleep(2 * time.Second) // accept, then just sit on it - never respond
	})
	defer closeFn()

	params, _ := json.Marshal(map[string]any{"port": port, "timeout_ms": 300})

	start := time.Now()
	res := UpgradeChecker{}.Run(context.Background(), NetConfig{}, "127.0.0.1", params)
	elapsed := time.Since(start)

	if res.Success {
		t.Fatal("Success = true, want false - server never responded")
	}
	if elapsed > 2*time.Second {
		t.Errorf("Run() took %v, want it bounded by the ~300ms timeout_ms, not hanging until the server closes", elapsed)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail (package doesn't compile - none of the referenced symbols exist yet)**

Run: `go test ./internal/checks/... -run 'TestMatchSwitchingProtocols|TestBuildUpgradeRequestWebsocket|TestUpgradeChecker'`
Expected: FAIL to build - `undefined: matchSwitchingProtocols`, `undefined: buildUpgradeRequest`, `undefined: UpgradeChecker`.

- [ ] **Step 3: Create `internal/checks/upgrade.go`**

```go
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
```

- [ ] **Step 4: Register `"upgrade"` in the checker registry**

Open `internal/checks/checks.go`, find the `registry` map, add the new entry:

```go
var registry = map[string]Checker{
	"ping":    PingChecker{},
	"tcp":     TCPChecker{},
	"http":    HTTPChecker{},
	"dns":     DNSChecker{},
	"tls":     TLSChecker{},
	"upgrade": UpgradeChecker{},
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go build ./... && go vet ./... && go test ./internal/checks/... -v`
Expected: BUILD OK, VET OK, all tests PASS - the 6 new tests (`TestMatchSwitchingProtocols` + 5 subtests, `TestBuildUpgradeRequestWebsocket`, `TestUpgradeCheckerMatchesRealSwitchingProtocols`, `TestUpgradeCheckerDoesNotMatchNormalResponse`, `TestUpgradeCheckerConnectionRefused`, `TestUpgradeCheckerTimeout`) and every pre-existing test in the package (no regressions).

- [ ] **Step 6: Commit**

```bash
git add internal/checks/upgrade.go internal/checks/upgrade_test.go internal/checks/checks.go
git commit -m "Add upgrade check type: HTTP 101 Switching Protocols probe on port 443

Mirrors check_plain_http.py's \"upgrade\" mode (a standalone recon script
kept in the repo root) as a proper internal/checks.Checker - plain TCP,
not TLS-wrapped, same dial pattern as tcp.go. Success means the host
answered 101, not that the connection merely succeeded.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
```

---

## Task 2: `POST /api/v1/server-upgrade-scan` (Go)

**Files:**
- Create: `internal/api/public/serverupgradescan.go`
- Create: `internal/api/public/serverupgradescan_test.go`
- Modify: `cmd/server/main.go` (route registration, near `server-ping`)
- Modify: `internal/api/openapi.yaml` (near the existing `/api/v1/server-ping` entry)

- [ ] **Step 1: Write the failing tests**

Create `internal/api/public/serverupgradescan_test.go`:

```go
package public

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func doUpgradeScan(h *Handler, body any) (*httptest.ResponseRecorder, serverUpgradeScanResponse, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, serverUpgradeScanResponse{}, err
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/server-upgrade-scan", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	h.ServerUpgradeScan(rec, req)

	var resp serverUpgradeScanResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			return rec, serverUpgradeScanResponse{}, err
		}
	}
	return rec, resp, nil
}

// TestServerUpgradeScanReturnsOneResultPerTarget: the endpoint's request
// shape only takes bare targets (port 443 is always used, matching the
// design's fixed-port decision - see
// docs/superpowers/specs/2026-08-09-http-101-upgrade-check-design.md), so
// this test can't stand up a fake server to get matched:true (that would
// need binding port 443, privileged and untestable in CI) - the real
// matching logic is already fully covered by
// internal/checks/upgrade_test.go's TestUpgradeCheckerMatchesRealSwitchingProtocols.
// This layer's job is to verify the HTTP plumbing: one result per target,
// in order, with matched:false when nothing answers.
func TestServerUpgradeScanReturnsOneResultPerTarget(t *testing.T) {
	h := &Handler{}
	_, resp, err := doUpgradeScan(h, map[string]any{
		"targets": []string{"127.0.0.1", "203.0.113.1"}, // 203.0.113.1 is TEST-NET-3 (RFC 5737) - never routable
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(resp.Results))
	}
	for i, want := range []string{"127.0.0.1", "203.0.113.1"} {
		if resp.Results[i].Target != want {
			t.Errorf("Results[%d].Target = %q, want %q", i, resp.Results[i].Target, want)
		}
		if resp.Results[i].Matched {
			t.Errorf("Results[%d].Matched = true, want false - nothing is listening/routable", i)
		}
	}
}

// TestServerUpgradeScanConcurrentRequestsDoNotCrossTalk fires several
// concurrent requests, each about a *different* target, and checks each
// response only ever contains its own request's target - never another
// goroutine's. Same regression shape as serverping_test.go's
// TestServerPingConcurrentRequestsDoNotCrossTalk (the cross-talk bug
// described in docs/superpowers/specs/2026-07-19-telegram-bot-merge-design.md),
// adapted for this endpoint's fixed port: all targets are distinct loopback
// addresses (127.0.0.2 .. 127.0.0.9 - the entire 127.0.0.0/8 block is
// loopback, so nothing listens on port 443 on any of them, and the OS
// refuses the connection immediately without any real network round-trip).
func TestServerUpgradeScanConcurrentRequestsDoNotCrossTalk(t *testing.T) {
	const n = 8
	h := &Handler{}
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			target := fmt.Sprintf("127.0.0.%d", i+2)
			_, resp, err := doUpgradeScan(h, map[string]any{"targets": []string{target}})
			if err != nil {
				errs[i] = fmt.Errorf("goroutine %d: %w", i, err)
				return
			}
			if len(resp.Results) != 1 {
				errs[i] = fmt.Errorf("goroutine %d: got %d results, want 1", i, len(resp.Results))
				return
			}
			if resp.Results[0].Target != target {
				errs[i] = fmt.Errorf("goroutine %d: requested target %s, response target=%s - a different request's result leaked in", i, target, resp.Results[0].Target)
			}
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}

func TestServerUpgradeScanEmptyTargets(t *testing.T) {
	h := &Handler{}
	rec, _, err := doUpgradeScan(h, map[string]any{"targets": []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestServerUpgradeScanTooManyTargets(t *testing.T) {
	targets := make([]string, serverUpgradeScanMaxTargets+1)
	for i := range targets {
		targets[i] = "127.0.0.1"
	}
	h := &Handler{}
	rec, _, err := doUpgradeScan(h, map[string]any{"targets": targets})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail (handler doesn't exist yet)**

Run: `go test ./internal/api/public/... -run TestServerUpgradeScan`
Expected: FAIL to build - `h.ServerUpgradeScan undefined`, `serverUpgradeScanResponse` undefined, `serverUpgradeScanMaxTargets` undefined.

- [ ] **Step 3: Create `internal/api/public/serverupgradescan.go`**

```go
// ServerUpgradeScan (POST /api/v1/server-upgrade-scan) probes whether each
// target answers HTTP 101 Switching Protocols to a plaintext
// Connection: Upgrade request on port 443. Synchronous, no node involved,
// no DB access - same "every request is fully self-contained, no shared
// mutable state across concurrent requests" guarantee as ServerPing (see
// serverping.go's own doc comment). See
// docs/superpowers/specs/2026-08-09-http-101-upgrade-check-design.md.
package public

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"pingachock/internal/api"
	"pingachock/internal/checks"
)

const (
	serverUpgradeScanMaxTargets = 100
	serverUpgradeScanTimeout    = 20 * time.Second
)

type serverUpgradeScanRequest struct {
	Targets []string `json:"targets"`
}

type serverUpgradeScanResult struct {
	Target  string `json:"target"`
	Matched bool   `json:"matched"`
}

type serverUpgradeScanResponse struct {
	Results []serverUpgradeScanResult `json:"results"`
}

func (h *Handler) ServerUpgradeScan(w http.ResponseWriter, r *http.Request) {
	var req serverUpgradeScanRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if len(req.Targets) == 0 {
		api.WriteError(w, http.StatusBadRequest, "targets must not be empty")
		return
	}
	if len(req.Targets) > serverUpgradeScanMaxTargets {
		api.WriteError(w, http.StatusBadRequest, fmt.Sprintf("targets: max %d per request", serverUpgradeScanMaxTargets))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), serverUpgradeScanTimeout)
	defer cancel()

	checker, _ := checks.Get("upgrade")
	results := make([]serverUpgradeScanResult, len(req.Targets))
	var wg sync.WaitGroup
	for i, target := range req.Targets {
		wg.Add(1)
		go func(i int, target string) {
			defer wg.Done()
			res := checker.Run(ctx, checks.NetConfig{}, target, json.RawMessage(`{}`))
			results[i] = serverUpgradeScanResult{Target: target, Matched: res.Success}
		}(i, target)
	}
	wg.Wait()

	api.WriteJSON(w, http.StatusOK, serverUpgradeScanResponse{Results: results})
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go build ./... && go test ./internal/api/public/... -run TestServerUpgradeScan -v`
Expected: BUILD OK, all 4 tests PASS (`TestServerUpgradeScanReturnsOneResultPerTarget`,
`TestServerUpgradeScanConcurrentRequestsDoNotCrossTalk`, `TestServerUpgradeScanEmptyTargets`,
`TestServerUpgradeScanTooManyTargets`). The first two may take a couple of seconds each
(real dial timeouts/refusals) - that's expected, not a bug.

- [ ] **Step 5: Register the route in `cmd/server/main.go`**

Find the existing line (near the other `RequireAPIKey`-scoped routes):
```go
			r.Post("/server-ping", publicH.ServerPing)
```
Add immediately after it:
```go
			r.Post("/server-upgrade-scan", publicH.ServerUpgradeScan)
```

- [ ] **Step 6: Run the full build**

Run: `go build ./... && go vet ./... && go test ./... 2>&1 | tail -30`
Expected: BUILD OK, VET OK, every package's tests PASS (no regressions anywhere else).

- [ ] **Step 7: Document the endpoint in `internal/api/openapi.yaml`**

Find the existing `/api/v1/server-ping:` entry (a top-level key under `paths:`). Add a new sibling entry immediately after its closing (before the next top-level path, e.g. `/api/v1/accounts:`):

```yaml
  /api/v1/server-upgrade-scan:
    post:
      summary: HTTP 101 (websocket upgrade) проверка напрямую с бекенда
      description: >
        Для каждой цели: голое TCP-соединение на порт 443 (без TLS), запрос
        с Connection: Upgrade / Upgrade: websocket, matched=true если ответ -
        HTTP 101 Switching Protocols. Синхронно, без узла, не создаёт
        checks/check_runs. См.
        docs/superpowers/specs/2026-08-09-http-101-upgrade-check-design.md.
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [targets]
              properties:
                targets:
                  type: array
                  items: { type: string }
                  maxItems: 100
            example:
              targets: ["1.2.3.4", "vpn.example.com"]
      responses:
        '200':
          description: Результат по каждой цели
          content:
            application/json:
              schema:
                type: object
                properties:
                  results:
                    type: array
                    items:
                      type: object
                      properties:
                        target: { type: string }
                        matched: { type: boolean }
              example:
                results:
                  - target: "1.2.3.4"
                    matched: true
                  - target: "vpn.example.com"
                    matched: false
        '400':
          $ref: '#/components/responses/Error'
```

- [ ] **Step 8: Commit**

```bash
git add internal/api/public/serverupgradescan.go internal/api/public/serverupgradescan_test.go cmd/server/main.go internal/api/openapi.yaml
git commit -m "Add POST /api/v1/server-upgrade-scan

Synchronous HTTP 101 (websocket upgrade) probe, port 443 fixed, up to 100
targets - same self-contained-per-request guarantee as /api/v1/server-ping,
including a concurrent-requests-don't-cross-talk regression test. Documented
in openapi.yaml as a first-class endpoint: this is meant to be used
directly via the API, not just through the bot.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
```

---

## Task 3: Bot API client (`scanUpgrade`)

**Files:**
- Modify: `bot/src/pingachock-client.ts`
- Create: `bot/src/upgrade-scan.test.ts`

- [ ] **Step 1: Write the failing test**

Create `bot/src/upgrade-scan.test.ts`:

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

const { mapUpgradeScanResults } = require('./pingachock-client') as typeof import('./pingachock-client');

test('maps a normal response', () => {
  const data = { results: [{ target: '1.2.3.4', matched: true }, { target: '5.6.7.8', matched: false }] };
  assert.deepEqual(mapUpgradeScanResults(data), [
    { target: '1.2.3.4', matched: true },
    { target: '5.6.7.8', matched: false }
  ]);
});

test('missing results array yields an empty list, not a throw', () => {
  assert.deepEqual(mapUpgradeScanResults({}), []);
  assert.deepEqual(mapUpgradeScanResults(null), []);
});

test('missing matched field on an entry defaults to false, not undefined', () => {
  assert.deepEqual(mapUpgradeScanResults({ results: [{ target: '1.2.3.4' }] }), [{ target: '1.2.3.4', matched: false }]);
});
```

- [ ] **Step 2: Run the test to verify it fails**

Run (from `bot/`): `npx tsx --test src/upgrade-scan.test.ts`
Expected: FAIL - `mapUpgradeScanResults is not a function` (or `undefined`).

- [ ] **Step 3: Add `mapUpgradeScanResults` and `scanUpgrade` to `bot/src/pingachock-client.ts`**

Add at the bottom of the file, after the `ping` export:

```ts
export type UpgradeScanResult = { target: string; matched: boolean };

// mapUpgradeScanResults is split out from scanUpgrade so the response
// mapping itself is unit-testable without a live backend - mirrors
// toRouter's role for listRouters.
export function mapUpgradeScanResults(data: any): UpgradeScanResult[] {
  const results = Array.isArray(data?.results) ? data.results : [];
  return results.map((r: any) => ({ target: String(r?.target ?? ''), matched: Boolean(r?.matched) }));
}

// scanUpgrade: HTTP 101 (websocket upgrade) check, always against the
// backend itself (see /api/v1/server-upgrade-scan) - there is no
// node-routed equivalent, this check only ever makes sense from the
// server's own vantage point. See
// docs/superpowers/specs/2026-08-09-http-101-upgrade-check-design.md.
export async function scanUpgrade(targets: string[]): Promise<UpgradeScanResult[]> {
  const data = await fetchWithAuth('/api/v1/server-upgrade-scan', 'POST', { targets }, 'api');
  return mapUpgradeScanResults(data);
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run (from `bot/`): `npx tsx --test src/upgrade-scan.test.ts`
Expected: PASS (3 tests, 0 failures).

- [ ] **Step 5: Type-check the whole bot**

Run (from `bot/`): `npx tsc --noEmit`
Expected: no output, exit code 0.

- [ ] **Step 6: Commit**

```bash
git add bot/src/pingachock-client.ts bot/src/upgrade-scan.test.ts
git commit -m "Bot: add scanUpgrade client for /api/v1/server-upgrade-scan

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
```

---

## Task 4: Bot UI ("➕ Дополнительные проверки" → "HTTP 101 check (Websocket)")

**Files:**
- Modify: `bot/src/index.ts`

This task has no isolated unit test (it's Telegraf callback/session wiring around the already-tested `scanUpgrade`/`mapUpgradeScanResults` and the already-tested `parseTargetsMultiline`) - verified via `tsc --noEmit` plus a manual runthrough against the real bot, same as every other menu flow in this file.

- [ ] **Step 1: Add the session flag**

In `bot/src/index.ts`, find the `MySession` type definition (starts with `type MySession = {`). Add a new field near the other `awaiting*Input`-style flags (e.g. right after `awaitingHealthListInput?: boolean;` and its associated fields):

```ts
  awaitingUpgradeScanTargets?: boolean;
```

- [ ] **Step 2: Add the third main-menu button**

Find `function mainMenuKeyboard() {` and change:

```ts
function mainMenuKeyboard() {
  return Markup.inlineKeyboard([
    [Markup.button.callback('🔍 Ping', 'menu:ping')],
    [Markup.button.callback('📊 Health report', 'menu:health')]
  ]);
}
```

to:

```ts
function mainMenuKeyboard() {
  return Markup.inlineKeyboard([
    [Markup.button.callback('🔍 Ping', 'menu:ping')],
    [Markup.button.callback('📊 Health report', 'menu:health')],
    [Markup.button.callback('➕ Дополнительные проверки', 'menu:extra')]
  ]);
}
```

- [ ] **Step 3: Add the submenu keyboard function**

Add right after `mainMenuKeyboard()`:

```ts
function extraChecksKeyboard() {
  return Markup.inlineKeyboard([
    [Markup.button.callback('HTTP 101 check (Websocket)', 'extra:http101')],
    [Markup.button.callback('◀️ Назад', 'menu:root')]
  ]);
}
```

- [ ] **Step 4: Reset the new flag in `menu:root`, and add the two new `bot.action` handlers**

Find:
```ts
bot.action('menu:root', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  ctx.session.awaitingPingInput = false;
  await ctx.answerCbQuery();
  await safeEditOrReply(ctx, await renderMainMenuText(), mainMenuKeyboard());
});
```
Change to:
```ts
bot.action('menu:root', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  ctx.session.awaitingPingInput = false;
  ctx.session.awaitingUpgradeScanTargets = false;
  await ctx.answerCbQuery();
  await safeEditOrReply(ctx, await renderMainMenuText(), mainMenuKeyboard());
});
```

Immediately after the `menu:root` handler (before `bot.action('menu:ping', ...)`), add:

```ts
bot.action('menu:extra', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  await ctx.answerCbQuery();
  ctx.session.awaitingUpgradeScanTargets = false;
  await safeEditOrReply(ctx, 'Дополнительные проверки:', extraChecksKeyboard());
});

bot.action('extra:http101', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  await ctx.answerCbQuery();
  ctx.session.awaitingUpgradeScanTargets = true;
  await safeEditOrReply(
    ctx,
    'Пришли список хостов для HTTP 101 check (IPv4, CIDR, диапазон или домен, по одному на строку либо через запятую). Порт 443, протокол websocket. Максимум 100 целей.',
    extraChecksKeyboard()
  );
});
```

- [ ] **Step 5: Add the text-input handler**

Find the `bot.on('text', async (ctx, next) => {` block. Locate the existing Health Report custom-list block (search for `awaitingHealthListInput && (await isAuthorizedUser(ctx))`) - add the new block immediately before it (order among independent `awaiting*` checks in this handler doesn't matter functionally, since only one flag is ever true at a time in normal use, but keeping related "paste a target list" flows next to each other aids readability):

```ts
  // Дополнительные проверки: HTTP 101 check — ждём список целей
  if (ctx.session.awaitingUpgradeScanTargets && (await isAuthorizedUser(ctx))) {
    const input = ctx.message.text.trim();
    let parsed: { targets: string[]; expanded: boolean } | null = null;
    try {
      parsed = parseTargetsMultiline(input);
    } catch (err) {
      const errMsg = err instanceof Error ? err.message : String(err);
      await ctx.reply(`Ошибка валидации целей: ${errMsg}`, extraChecksKeyboard());
      return;
    }

    if (!parsed) {
      await ctx.reply(
        'Неверный формат. Пришли IPv4, подсеть (CIDR), диапазон IPv4 или домен. Каждый объект с новой строки.',
        extraChecksKeyboard()
      );
      return;
    }

    if (parsed.targets.length > 100) {
      await ctx.reply(
        `Слишком много целей: ${parsed.targets.length}, максимум 100. Пришли список короче.`,
        extraChecksKeyboard()
      );
      return;
    }

    ctx.session.awaitingUpgradeScanTargets = false;

    try {
      const results = await apiClient.scanUpgrade(parsed.targets);
      const lines = results.map((r) => `${r.matched ? '✅' : '❌'} ${r.target}`);
      const matchedCount = results.filter((r) => r.matched).length;
      const reportText =
        `HTTP 101 check (Websocket)\n` +
        `Время проверки: ${formatHumanDate(new Date())}\n\n` +
        `${lines.join('\n')}\n\n` +
        `Итог: ${matchedCount} из ${results.length} отвечают условию`;

      const sendAsFile = reportText.length > TELEGRAM_SAFE_TEXT_LIMIT;
      if (sendAsFile) {
        const filename = `http101_${safeFilenameDate(new Date())}.txt`;
        await (ctx as any).replyWithDocument({ source: Buffer.from(reportText, 'utf8'), filename });
        await ctx.reply('Готово.', extraChecksKeyboard());
      } else {
        await ctx.reply(reportText, extraChecksKeyboard());
      }
    } catch (err) {
      const errMsg = err instanceof Error ? err.message : String(err);
      await ctx.reply(`Ошибка:\n${errMsg}`, extraChecksKeyboard());
    }
    return;
  }

```

- [ ] **Step 6: Type-check**

Run (from `bot/`): `npx tsc --noEmit`
Expected: no output, exit code 0. If `parseTargetsMultiline`, `formatHumanDate`, `safeFilenameDate`, `TELEGRAM_SAFE_TEXT_LIMIT`, or `extraChecksKeyboard` are reported as undefined/out of scope, check they're defined at module level in `bot/src/index.ts` (they all already exist except `extraChecksKeyboard`, added in Step 3) and not accidentally scoped inside another function.

- [ ] **Step 7: Production build**

Run (from `bot/`): `npm run build`
Expected: `tsc -p tsconfig.json` completes with no errors.

- [ ] **Step 8: Run the full bot pure-logic test suite (regression check)**

Run (from `bot/`): `npx tsx --test src/*.test.ts`
Expected: every test passes except `pingachock-client.test.ts` (needs a live backend + `TEST_API_KEY`, fails at its own `before()` hook if none is configured - this is expected and pre-existing, not a regression).

- [ ] **Step 9: Commit**

```bash
git add bot/src/index.ts
git commit -m "Bot: wire up 'Дополнительные проверки' -> HTTP 101 check (Websocket) menu

New third main-menu button, always targets the server node (no router
picker - there is no node-routed equivalent for this check), reuses the
existing parseTargetsMultiline for input and the sendAsFile/chunked-report
convention already used everywhere else in the bot.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
```

---

## Task 5: Final verification and manual smoke test

**Files:** none (verification only)

- [ ] **Step 1: Full Go verification**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: BUILD OK, VET OK, every package's tests pass.

- [ ] **Step 2: Full bot verification**

Run (from `bot/`): `npx tsc --noEmit && npm run build && npx tsx --test src/*.test.ts`
Expected: clean type-check, clean build, all tests pass except the live-backend-only `pingachock-client.test.ts` (expected, see Task 4 Step 8).

- [ ] **Step 3: Manual smoke test against a real backend, if one is running locally or reachable**

If a backend is reachable (check with `curl -s -o /dev/null -w "%{http_code}\n" <API_URL>/healthz`), exercise the new endpoint directly:

```bash
curl -s -X POST <API_URL>/api/v1/server-upgrade-scan \
  -H "Authorization: Bearer <api_key>" -H "Content-Type: application/json" \
  -d '{"targets":["127.0.0.1"]}'
```
Expected: `{"results":[{"target":"127.0.0.1","matched":false}]}` (nothing real listens on 443 on the backend's loopback) - confirms the route is wired, auth works, and the response shape matches the spec. If a real candidate relay host is known to answer 101, test against it too and confirm `matched: true`.

If no backend is reachable in this environment, skip this step and note it explicitly when reporting completion - do not fabricate a result.

- [ ] **Step 4: Update the design spec's status**

In `docs/superpowers/specs/2026-08-09-http-101-upgrade-check-design.md`, change the `Status:` line from:
```
Status: APPROVED, ready for implementation planning.
```
to:
```
Status: DONE. Implemented per docs/superpowers/plans/2026-08-09-http-101-upgrade-check.md.
```

- [ ] **Step 5: Commit and push**

```bash
git add docs/superpowers/specs/2026-08-09-http-101-upgrade-check-design.md
git commit -m "Mark HTTP 101 upgrade check design DONE

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
git push origin main
```
