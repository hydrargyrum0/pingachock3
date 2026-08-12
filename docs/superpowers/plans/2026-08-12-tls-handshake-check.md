# TLS Handshake Speed Check Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Measure TLS handshake speed against a candidate relay IP (presenting a chosen SNI) from a real measurement node, plus a placeholder target the user's own relay forwards decrypted traffic to.

**Architecture:** A tiny new standalone binary (`cmd/handshaketarget`) that does nothing but complete a WebSocket opening handshake and hold the connection open, deployed as its own Docker Compose service. The check itself reuses the existing `internal/checks/tls.go` `TLSChecker` unchanged, dispatched through the same `/api/v1/checks` + `node_selector` mechanism every other node-routed check uses - no new backend code at all. A bot flow ties the two together.

**Tech Stack:** Go (`net/http`, `crypto/sha1` for the RFC 6455 handshake), Docker Compose, TypeScript/Telegraf (bot).

**Spec:** `docs/superpowers/specs/2026-08-12-tls-handshake-check-design.md`

**Scope note (stated plainly, matching the spec's own callout):** node selection is `auto` or one specific node, no `ALL` fan-out - same reasoning as the VLESS Speedtest feature (`docs/superpowers/plans/2026-08-12-vless-speedtest-check.md`), which this plan's bot UI mirrors closely.

---

## Task 1: Placeholder target (`cmd/handshaketarget`)

**Files:**
- Create: `cmd/handshaketarget/main.go`
- Create: `cmd/handshaketarget/main_test.go`

- [ ] **Step 1: Write the failing tests**

Create `cmd/handshaketarget/main_test.go`:

```go
package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func dialAndUpgrade(t *testing.T, addr string, key string) (*bufio.Reader, net.Conn) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	req := "GET / HTTP/1.1\r\n" +
		"Host: " + addr + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write request: %v", err)
	}
	return bufio.NewReader(conn), conn
}

func TestHandleUpgradeCompletesRealHandshake(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(handleUpgrade))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	const key = "dGhlIHNhbXBsZSBub25jZQ=="
	reader, conn := dialAndUpgrade(t, addr, key)
	defer conn.Close()

	statusLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	if !strings.Contains(statusLine, "101") {
		t.Fatalf("status line = %q, want 101 Switching Protocols", statusLine)
	}

	var acceptHeader string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read headers: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "Sec-WebSocket-Accept:") {
			acceptHeader = strings.TrimSpace(strings.TrimPrefix(line, "Sec-WebSocket-Accept:"))
		}
	}

	sum := sha1.Sum([]byte(key + wsMagicGUID))
	want := base64.StdEncoding.EncodeToString(sum[:])
	if acceptHeader != want {
		t.Errorf("Sec-WebSocket-Accept = %q, want %q", acceptHeader, want)
	}
}

func TestHandleUpgradeRejectsNonUpgradeRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(handleUpgrade))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail (package doesn't exist yet)**

Run: `go test ./cmd/handshaketarget/... 2>&1`
Expected: FAIL to build - `no Go files in .../cmd/handshaketarget` (the
directory doesn't exist yet).

- [ ] **Step 3: Create `cmd/handshaketarget/main.go`**

```go
// cmd/handshaketarget is a placeholder WebSocket-upgrading TCP listener -
// see docs/superpowers/specs/2026-08-12-tls-handshake-check-design.md. It
// exists purely as something for a user's own TLS-terminating relay to
// forward decrypted traffic to when measuring TLS handshake speed against
// that relay (internal/checks/tls.go's TLSChecker, dispatched from the
// bot's "TLS Handshake" flow) - it has no real function beyond completing
// the WebSocket opening handshake and holding the connection open. No TLS
// here: whatever fronts this already terminated TLS and forwards
// plaintext.
package main

import (
	"crypto/sha1"
	"encoding/base64"
	"log"
	"net/http"
	"os"
	"strings"
)

const wsMagicGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

func handleUpgrade(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		http.Error(w, "expected a websocket upgrade request", http.StatusBadRequest)
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}
	conn, buf, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()

	sum := sha1.Sum([]byte(key + wsMagicGUID))
	accept := base64.StdEncoding.EncodeToString(sum[:])

	buf.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	buf.WriteString("Upgrade: websocket\r\n")
	buf.WriteString("Connection: Upgrade\r\n")
	buf.WriteString("Sec-WebSocket-Accept: " + accept + "\r\n\r\n")
	if err := buf.Flush(); err != nil {
		return
	}

	// Hold the connection open, discarding whatever arrives, until the
	// peer closes it - there is nothing else for this placeholder to do.
	discard := make([]byte, 4096)
	for {
		if _, err := conn.Read(discard); err != nil {
			return
		}
	}
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "1343"
	}
	log.Printf("handshake-target listening on :%s", port)
	if err := http.ListenAndServe(":"+port, http.HandlerFunc(handleUpgrade)); err != nil {
		log.Fatal(err)
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go build ./... && go vet ./... && go test ./cmd/handshaketarget/... -v`
Expected: BUILD OK, VET OK, both tests PASS
(`TestHandleUpgradeCompletesRealHandshake`,
`TestHandleUpgradeRejectsNonUpgradeRequest`).

- [ ] **Step 5: Run the full test suite (regression check)**

Run: `go test ./... 2>&1 | tail -20`
Expected: every package's tests still pass - this task touches no
existing file.

- [ ] **Step 6: Commit**

```bash
git add cmd/handshaketarget/main.go cmd/handshaketarget/main_test.go
git commit -m "Add cmd/handshaketarget: placeholder WS target for TLS handshake checks

Completes the RFC 6455 opening handshake (101 Switching Protocols +
correct Sec-WebSocket-Accept) and then just holds the connection open,
discarding everything - no real function, per the design's own
requirement. Plain HTTP/WS, no TLS: whatever relay fronts this already
terminated TLS and forwards plaintext.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
```

---

## Task 2: Deploy as its own Compose service

**Files:**
- Create: `Dockerfile.handshaketarget`
- Modify: `docker-compose.prod.yml`

- [ ] **Step 1: Create `Dockerfile.handshaketarget`**

```dockerfile
# Placeholder WebSocket target for TLS-handshake-speed checks - see
# docs/superpowers/specs/2026-08-12-tls-handshake-check-design.md. No real
# function beyond completing the WS opening handshake; doesn't import
# internal/, doesn't need ca-certificates (makes no outbound TLS calls).
FROM golang:1.26-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/handshaketarget ./cmd/handshaketarget
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/handshaketarget ./cmd/handshaketarget

FROM alpine:3.20
COPY --from=build /out/handshaketarget /usr/local/bin/handshaketarget
EXPOSE 1343
ENTRYPOINT ["/usr/local/bin/handshaketarget"]
```

- [ ] **Step 2: Verify the image builds**

Run: `docker build -f Dockerfile.handshaketarget -t pingachock3-handshaketarget-test .`
Expected: builds successfully through both stages (much faster than the
backend image - no xray fetch, no `internal/` copy).

- [ ] **Step 3: Smoke-test the built image runs and completes a real handshake**

Run:
```bash
docker run -d --rm -p 12343:1343 --name handshaketarget-smoketest pingachock3-handshaketarget-test
```
Then, from `internal/checks` (any directory with a shell), verify a plain
TCP+HTTP client completes the handshake - the simplest available way to do
that with tools already used in this repo is `curl`'s own WebSocket
upgrade support:
```bash
curl -sv -N -H "Connection: Upgrade" -H "Upgrade: websocket" -H "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==" -H "Sec-WebSocket-Version: 13" http://localhost:12343/ --max-time 2
```
Expected: the verbose output (`-v`) shows `< HTTP/1.1 101 Switching
Protocols` and a `Sec-WebSocket-Accept` header - curl then hangs waiting
for more data until the 2s `--max-time` cuts it off (expected: the
placeholder never sends anything else, per its own design). Clean up:
```bash
docker stop handshaketarget-smoketest
docker rmi pingachock3-handshaketarget-test
```

- [ ] **Step 4: Add the service to `docker-compose.prod.yml`**

Read `docker-compose.prod.yml` first to confirm its current exact
contents, then add a new service after the existing `caddy:` block (before
the top-level `networks:` key):

```yaml
  handshake-target:
    build: { context: ., dockerfile: Dockerfile.handshaketarget }
    restart: unless-stopped
    ports:
      - "1343:1343"
```

(No `depends_on`, no `networks: [internal]` entry - this is fully
standalone, published straight to the host since whatever terminates TLS
for the user's chosen domain lives outside this compose stack entirely
and needs to reach this port from outside the container network. See the
design doc's own "Published to the host" reasoning.)

- [ ] **Step 5: Validate the compose file**

The file's existing required variables (`POSTGRES_PASSWORD`,
`ADMIN_TOKEN`, `TELEGRAM_BOT_TOKEN`, `TELEGRAM_BOT_ADMIN_ID`) use `:?set
in .env` - Compose errors out if they're unset, which they are in this
dev environment (no real `.env` here). Supply throwaway values inline
purely to validate syntax/service-references, not to actually run
anything:

Run: `POSTGRES_PASSWORD=x ADMIN_TOKEN=x TELEGRAM_BOT_TOKEN=x TELEGRAM_BOT_ADMIN_ID=x docker compose -f docker-compose.prod.yml config --quiet`
Expected: no output, exit code 0.

- [ ] **Step 6: Commit**

```bash
git add Dockerfile.handshaketarget docker-compose.prod.yml
git commit -m "Deploy handshake-target as its own Compose service

Independent of backend/bot/postgres/caddy - no depends_on, published
straight to the host on 1343 since whatever terminates TLS for the user's
chosen domain lives outside this compose stack. Deploying it doesn't
require restarting anything else.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
```

---

## Task 3: Bot API client (`checkTlsHandshake`)

**Files:**
- Modify: `bot/src/pingachock-client.ts`
- Create: `bot/src/tls-handshake.test.ts`

- [ ] **Step 1: Write the failing test**

Create `bot/src/tls-handshake.test.ts`:

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

const { parseTlsHandshakeTarget, mapTlsHandshakeResult } = require('./pingachock-client') as typeof import('./pingachock-client');

test('parses "IP SNI" separated by a space', () => {
  assert.deepEqual(parseTlsHandshakeTarget('123.123.123.123 pingachock.com'), {
    ip: '123.123.123.123',
    sni: 'pingachock.com'
  });
});

test('parses "IP, SNI" separated by a comma', () => {
  assert.deepEqual(parseTlsHandshakeTarget('123.123.123.123, pingachock.com'), {
    ip: '123.123.123.123',
    sni: 'pingachock.com'
  });
});

test('rejects a domain in the IP position', () => {
  assert.equal(parseTlsHandshakeTarget('example.com pingachock.com'), null);
});

test('rejects fewer or more than two tokens', () => {
  assert.equal(parseTlsHandshakeTarget('123.123.123.123'), null);
  assert.equal(parseTlsHandshakeTarget('123.123.123.123 pingachock.com extra'), null);
});

test('accepts a literal IPv6 address too', () => {
  assert.deepEqual(parseTlsHandshakeTarget('2001:db8::1 pingachock.com'), {
    ip: '2001:db8::1',
    sni: 'pingachock.com'
  });
});

test('successful run extracts latency_ms from the matching node_id run', () => {
  const check = {
    runs: [
      { node_id: 'other-node', result: { success: true, latency_ms: 999 } },
      { node_id: 'my-node', result: { success: true, latency_ms: 245 } }
    ]
  };
  const got = mapTlsHandshakeResult(check, 'my-node');
  assert.equal(got.success, true);
  assert.equal(got.latencyMs, 245);
});

test('failed run carries a translated error message, not the raw token', () => {
  const check = { runs: [{ node_id: 'my-node', result: { success: false, error_message: 'timeout' } }] };
  const got = mapTlsHandshakeResult(check, 'my-node');
  assert.equal(got.success, false);
  assert.equal(got.errorMessage, 'таймаут');
});

test('no matching run for this node_id is treated as no response', () => {
  const check = { runs: [{ node_id: 'other-node', result: { success: true, latency_ms: 1 } }] };
  const got = mapTlsHandshakeResult(check, 'my-node');
  assert.equal(got.success, false);
  assert.equal(got.errorMessage, 'нет ответа от узла');
});
```

- [ ] **Step 2: Run the test to verify it fails**

Run (from `bot/`): `npx tsx --test src/tls-handshake.test.ts`
Expected: FAIL - `parseTlsHandshakeTarget is not a function`.

- [ ] **Step 3: Add `parseTlsHandshakeTarget`, `mapTlsHandshakeResult`, and `checkTlsHandshake` to `bot/src/pingachock-client.ts`**

Add at the bottom of the file, after the `checkVlessSpeed` export added by
the VLESS Speedtest feature:

```ts
// parseTlsHandshakeTarget: "IP SNI" (or "IP, SNI"), exactly two tokens.
// The first must be a literal IP (net.isIP) - matches the design's own
// scenario (dialing a relay's real IP directly), not a domain. The second
// is the SNI, accepted as-is with no further validation - it's just a
// hostname the caller is asserting their own relay/DNS config makes sense
// of, this package has no way to check that.
export function parseTlsHandshakeTarget(input: string): { ip: string; sni: string } | null {
  const tokens = input.trim().split(/[\s,]+/).filter(Boolean);
  if (tokens.length !== 2) return null;
  const [ip, sni] = tokens;
  if (net.isIP(ip) === 0) return null;
  return { ip, sni };
}

export type TlsHandshakeResult = { success: boolean; latencyMs?: number; errorMessage?: string };

// mapTlsHandshakeResult is split out from checkTlsHandshake so the
// response-mapping itself is unit-testable without a live backend -
// mirrors mapVlessSpeedTestResult's role for checkVlessSpeed.
export function mapTlsHandshakeResult(check: any, nodeId: string): TlsHandshakeResult {
  const run = check?.runs?.find((r: any) => r.node_id === nodeId);
  const result = run?.result;
  if (!result) {
    return { success: false, errorMessage: 'нет ответа от узла' };
  }
  if (!result.success) {
    return { success: false, errorMessage: translateCheckError(result.error_message) ?? 'ошибка' };
  }
  return { success: true, latencyMs: typeof result.latency_ms === 'number' ? result.latency_ms : undefined };
}

// checkTlsHandshake: dispatches a "tls" check to one node - always a real
// node, never "server" (there is no server-side equivalent, this only
// means something from a node's own network vantage point).
// allow_insecure is fixed true - only handshake speed matters here, not
// certificate trust (the placeholder's own TLS termination is the user's
// relay, not this repo). See
// docs/superpowers/specs/2026-08-12-tls-handshake-check-design.md.
export async function checkTlsHandshake(ip: string, sni: string, routerName: string): Promise<TlsHandshakeResult> {
  const { id: nodeId } = await resolveNodeId(routerName);
  const created = (await fetchWithAuth(
    '/api/v1/checks',
    'POST',
    {
      type: 'tls',
      targets: [ip],
      params: { port: 443, sni, allow_insecure: true },
      node_selector: { node_ids: [nodeId] }
    },
    'api'
  )) as any;
  const checkId = created.batch_id ? created.checks[0].id : created.id;
  const check = await pollCheckUntilDone(checkId);
  return mapTlsHandshakeResult(check, nodeId);
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run (from `bot/`): `npx tsx --test src/tls-handshake.test.ts`
Expected: PASS (8 tests, 0 failures).

- [ ] **Step 5: Type-check and regression-test the whole bot**

Run (from `bot/`): `npx tsc --noEmit && npx tsx --test src/*.test.ts`
Expected: clean type-check; every test passes except the live-backend-only
`pingachock-client.test.ts` (pre-existing, expected).

- [ ] **Step 6: Commit**

```bash
git add bot/src/pingachock-client.ts bot/src/tls-handshake.test.ts
git commit -m "Bot: add checkTlsHandshake client

Dispatches a 'tls' check (internal/checks/tls.go's existing TLSChecker,
unchanged) to one node, port 443 + allow_insecure fixed. parseTlsHandshakeTarget
validates the first token as a literal IP - matches the design's own
scenario of dialing a relay's real IP directly, not a domain.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
```

---

## Task 4: Bot UI ("Дополнительные проверки" → "TLS Handshake")

**Files:**
- Modify: `bot/src/index.ts`

Mirrors the VLESS Speedtest feature's own router-toggle-then-type shape
exactly, with its own independent session state. No isolated unit test
(pure Telegraf wiring around the already-tested `checkTlsHandshake`) -
verified via `tsc --noEmit` + manual runthrough.

- [ ] **Step 1: Add session state**

In `bot/src/index.ts`, find the `MySession` type. Add these fields near
the `awaitingVlessConfig`/`vlessRouterIndex`/`vlessRouters` block added by
the VLESS Speedtest feature:

```ts
  awaitingTlsHandshakeTarget?: boolean;
  tlsHandshakeRouterIndex?: number;
  tlsHandshakeRouters?: Router[];
```

- [ ] **Step 2: Add a TLS-Handshake-specific router option resolver and keyboard**

Add right after `getVlessRouterOption`/`vlessKeyboard`:

```ts
// getTlsHandshakeRouterOption/tlsHandshakeKeyboard: own independent
// router-toggle state, no "ALL" - same reasoning as VLESS Speedtest's own
// picker (one chosen node at a time).
function getTlsHandshakeRouterOption(session: MySession): { label: string; value: string } {
  const routers = session.tlsHandshakeRouters ?? [];
  const options: Array<{ label: string; value: string }> = [
    { label: pingRouterLabels.auto, value: 'auto' },
    ...routers.map((r) => ({ label: r.name, value: r.name }))
  ];
  const index = session.tlsHandshakeRouterIndex ?? 0;
  return options[Math.max(0, Math.min(index, options.length - 1))];
}

function tlsHandshakeKeyboard(session: MySession) {
  const routerOpt = getTlsHandshakeRouterOption(session);
  return Markup.inlineKeyboard([
    [Markup.button.callback(routerOpt.label, 'extra:tls_toggle_router')],
    [Markup.button.callback('◀️ Назад', 'menu:root')]
  ]);
}
```

- [ ] **Step 3: Add the third "Дополнительные проверки" button**

Find `extraChecksKeyboard()` (now has two entries: HTTP 101 check and
VLESS Speedtest) and add a third row:

```ts
function extraChecksKeyboard() {
  return Markup.inlineKeyboard([
    [Markup.button.callback('HTTP 101 check (Websocket)', 'extra:http101')],
    [Markup.button.callback('VLESS Speedtest', 'extra:vlessspeedtest')],
    [Markup.button.callback('TLS Handshake', 'extra:tlshandshake')],
    [Markup.button.callback('◀️ Назад', 'menu:root')]
  ]);
}
```

- [ ] **Step 4: Add the action handlers**

Find the `extra:vless_toggle_router` handler (added by the VLESS
Speedtest feature) and add these two new handlers right after it:

```ts
bot.action('extra:tlshandshake', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  await ctx.answerCbQuery();

  ctx.session.tlsHandshakeRouterIndex = ctx.session.tlsHandshakeRouterIndex ?? 0;
  try {
    const allRouters = await apiClient.listRouters();
    ctx.session.tlsHandshakeRouters = allRouters.filter((r) => r.status === 'online');
    const optionsLen = 1 + (ctx.session.tlsHandshakeRouters?.length ?? 0);
    if (optionsLen > 0 && (ctx.session.tlsHandshakeRouterIndex ?? 0) >= optionsLen) {
      ctx.session.tlsHandshakeRouterIndex = 0;
    }
  } catch (err) {
    const errMsg = err instanceof Error ? err.message : String(err);
    await safeEditOrReply(ctx, `Ошибка:\n${errMsg}`, extraChecksKeyboard());
    return;
  }

  ctx.session.awaitingTlsHandshakeTarget = true;
  await safeEditOrReply(
    ctx,
    'Выбери узел (кнопка выше) и пришли цель: IP и домен (SNI) через пробел, например: 123.123.123.123 pingachock.com',
    tlsHandshakeKeyboard(ctx.session)
  );
});

bot.action('extra:tls_toggle_router', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  await ctx.answerCbQuery();
  const optionsLen = 1 + (ctx.session.tlsHandshakeRouters?.length ?? 0);
  ctx.session.tlsHandshakeRouterIndex = ((ctx.session.tlsHandshakeRouterIndex ?? 0) + 1) % Math.max(1, optionsLen);
  await safeEditOrIgnore(
    ctx,
    'Выбери узел (кнопка выше) и пришли цель: IP и домен (SNI) через пробел, например: 123.123.123.123 pingachock.com',
    tlsHandshakeKeyboard(ctx.session)
  );
});
```

Also update `menu:extra` and `menu:root` (both already reset
`awaitingUpgradeScanTargets`/`awaitingVlessConfig`) to reset the new flag
too:

```ts
bot.action('menu:root', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  ctx.session.awaitingPingInput = false;
  ctx.session.awaitingUpgradeScanTargets = false;
  ctx.session.awaitingVlessConfig = false;
  ctx.session.awaitingTlsHandshakeTarget = false;
  await ctx.answerCbQuery();
  await safeEditOrReply(ctx, await renderMainMenuText(), mainMenuKeyboard());
});
```

```ts
bot.action('menu:extra', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  await ctx.answerCbQuery();
  ctx.session.awaitingUpgradeScanTargets = false;
  ctx.session.awaitingVlessConfig = false;
  ctx.session.awaitingTlsHandshakeTarget = false;
  await safeEditOrReply(ctx, 'Дополнительные проверки:', extraChecksKeyboard());
});
```

- [ ] **Step 5: Add the text-input handler**

Find the `bot.on('text', async (ctx, next) => {` block. Locate the VLESS
Speedtest feature's own input block (search for `awaitingVlessConfig &&
(await isAuthorizedUser(ctx))`) and add this new block immediately before
it:

```ts
  // Дополнительные проверки: TLS Handshake — ждём "IP SNI"
  if (ctx.session.awaitingTlsHandshakeTarget && (await isAuthorizedUser(ctx))) {
    const input = ctx.message.text.trim();
    const parsed = apiClient.parseTlsHandshakeTarget(input);
    if (!parsed) {
      await ctx.reply(
        'Неверный формат. Пришли IP и домен (SNI) через пробел или запятую, например: 123.123.123.123 pingachock.com',
        tlsHandshakeKeyboard(ctx.session)
      );
      return;
    }

    const routerOpt = getTlsHandshakeRouterOption(ctx.session);
    ctx.session.awaitingTlsHandshakeTarget = false;

    try {
      const result = await apiClient.checkTlsHandshake(parsed.ip, parsed.sni, routerOpt.value);
      const reportText = result.success
        ? `TLS Handshake Check\nВремя проверки: ${formatHumanDate(new Date())}\nЦель: ${parsed.ip}:443, SNI: ${parsed.sni}\nУзел: ${routerOpt.label}\n\n✅ ${result.latencyMs != null ? result.latencyMs + ' ms' : 'успех, время не измерено'}`
        : `TLS Handshake Check\nВремя проверки: ${formatHumanDate(new Date())}\nЦель: ${parsed.ip}:443, SNI: ${parsed.sni}\nУзел: ${routerOpt.label}\n\n❌ ошибка: ${result.errorMessage ?? 'неизвестная ошибка'}`;
      await ctx.reply(reportText, extraChecksKeyboard());
    } catch (err) {
      const errMsg = err instanceof Error ? err.message : String(err);
      await ctx.reply(`Ошибка:\n${errMsg}`, extraChecksKeyboard());
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
git commit -m "Bot: wire up 'Дополнительные проверки' -> TLS Handshake menu

Third entry in the extra-checks submenu, same router-toggle-then-type
shape as VLESS Speedtest (own independent session state, no ALL option).
Input is 'IP SNI' in one message, validated via parseTlsHandshakeTarget
before dispatch.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
```

---

## Task 5: Final verification

**Files:** none (verification only)

- [ ] **Step 1: Full Go verification**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: BUILD OK, VET OK, every package's tests pass.

- [ ] **Step 2: Full bot verification**

Run (from `bot/`): `npx tsc --noEmit && npm run build && npx tsx --test src/*.test.ts`
Expected: clean type-check, clean build, all tests pass except the
live-backend-only `pingachock-client.test.ts`.

- [ ] **Step 3: Manual smoke test, if a reachable backend + a real relay setup are available**

This needs live infrastructure this repo doesn't stand up for automated
tests: a running backend, a node online and polling it, and the user's
own relay actually forwarding to a deployed `handshake-target`. If
available, exercise the full flow through the bot (➕ Дополнительные
проверки → TLS Handshake) with a real target IP + SNI and confirm a
plausible latency comes back. If nothing is reachable in this environment,
skip this step and say so explicitly when reporting completion - do not
fabricate a result.

- [ ] **Step 4: Update the design spec's status**

In `docs/superpowers/specs/2026-08-12-tls-handshake-check-design.md`,
change:
```
Status: APPROVED, ready for implementation planning.
```
to:
```
Status: DONE. Implemented per docs/superpowers/plans/2026-08-12-tls-handshake-check.md.
```

- [ ] **Step 5: Commit and push**

```bash
git add docs/superpowers/specs/2026-08-12-tls-handshake-check-design.md
git commit -m "Mark TLS handshake check design DONE

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
git push origin main
```
