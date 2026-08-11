# HTTP 101 upgrade check ("Дополнительные проверки") — design

Status: APPROVED, ready for implementation planning.

## Problem

`check_plain_http.py` (a standalone recon script the user keeps in the repo
root, not part of the shipped product) has an `upgrade` mode: it opens a
plain TCP connection to a host on port 443, sends an HTTP/1.1 request with
`Connection: Upgrade` / `Upgrade: websocket`, and records whether the host
answers `101 Switching Protocols` - i.e. whether it will blindly upgrade the
protocol on a port that's supposed to be TLS-only. This is useful for
finding candidate relay/VPN-endpoint hosts (a host that upgrades on a raw,
unauthenticated request is a signal about how permissively it's configured).

The user wants the same specific check (upgrade/101 only - not the
script's other `signature`/`discover`/`pipeline` modes) available as part
of pingachock itself: reachable primarily via the API directly (the user's
main use case), and secondarily through the bot's UI.

## Scope

In scope: the `upgrade` check against a user-supplied list of up to 100
targets, port 443 and protocol `websocket` fixed (not configurable per
request - matches the script's own default and the one scenario the user
cares about). Always executed from the backend itself (the "server"
virtual node - see `internal/serveragent`), never dispatched to a real
node.

Out of scope (explicitly, not deferred - no plan to build these here):
the script's `signature` mode, `discover`/masscan-based host discovery,
`pipeline`, and any configurable port/protocol/concurrency knobs. The
script itself keeps working standalone for those.

## Architecture

### Go: `internal/checks/upgrade.go` - new `Checker`

New check type `"upgrade"`, registered in `internal/checks/checks.go`'s
registry alongside `ping`/`tcp`/`http`/`dns`/`tls`, same `Checker`
interface (`Run(ctx, netCfg, target, params) Result`).

Deliberately **not** TLS-wrapped - a plain `net.Dialer` TCP connection to
`target:port`, same dial pattern as `internal/checks/tcp.go` (reusing
`resolveIP`/`localAddr` the same way), not `tls.go`'s `tls.Client`. The
entire point of the check is whether a plaintext request on the
TLS-conventional port gets upgraded; wrapping it in TLS would test
something else entirely.

Params (all optional, sensible defaults - the Go type stays configurable
even though nothing above it will pass values yet, consistent with how
every other checker in this package works):
```go
type upgradeParams struct {
    Port      int    `json:"port"`       // default 443
    Protocol  string `json:"protocol"`   // default "websocket"
    TimeoutMs int     `json:"timeout_ms"` // default 5000
}
```

Request building mirrors `build_upgrade_request` in `check_plain_http.py`:
`GET / HTTP/1.1`, `Host: <target>`, a browser-like `User-Agent`,
`Connection: Upgrade`, `Upgrade: <protocol>`, plus protocol-specific
headers required for a real server to actually answer 101 instead of
400/426 - `Sec-WebSocket-Version: 13` + a random `Sec-WebSocket-Key` for
`websocket` (the only protocol actually reachable through the fixed
port/protocol default, but the type stays general).

Response reading is capped (mirrors the script's `--max-body`, default
16 KiB) - the status line arrives in the first bytes, no reason to trust a
misbehaving host to ever close the connection. `Result.Success` = true iff
the status line is `HTTP/x.y 101 ...` (mirrors `match_switching_protocols`
in the script). No `LatencyMs` - matching isn't a timing check, `Success`
alone is the answer.

Errors get the same `classifyNetError` treatment as `tcp.go`/`tls.go`
(timeout, connection refused, dns resolution failed, ...) - no raw Go
error text leaks here either, consistent with
`docs/superpowers/specs/2026-07-25-ping-result-classification-design.md`.

### Backend: `POST /api/v1/server-upgrade-scan`

New handler in `internal/api/public`, same file-level pattern as
`serverping.go` (`internal/api/public/serverping.go`): fully synchronous,
touches no DB, no check_runs - runs `checks.Get("upgrade")` directly per
target in its own goroutine inside the request handler, same "every
request is self-contained, no shared mutable state" guarantee server-ping
already documents (this is the same correctness requirement from the
original bot-merge design: concurrent requests from different users must
never cross-contaminate).

```
POST /api/v1/server-upgrade-scan
Auth: Bearer <api_key>          (RequireAPIKey - same tier as server-ping)

Request:
{ "targets": ["1.2.3.4", "vpn.example.com", ...] }   // 1..100 entries

Response 200:
{ "results": [
    { "target": "1.2.3.4", "matched": true },
    { "target": "vpn.example.com", "matched": false }
] }

Response 400: empty targets, more than 100 targets, or malformed body -
same error shape as server-ping (`api.WriteError`).
```

`serverUpgradeScanMaxTargets = 100` (a named constant, mirroring
`serverPingMaxTargets = 50` in the same package - different limit because
this endpoint has exactly one fixed-cost check per target, not up to
`icmp + N ports` like server-ping). `serverUpgradeScanTimeout` bounds the
whole request (20s, same value as `serverPingTimeout` - each target's own
check has its own ~5s budget and all targets run concurrently, so total
wall time is bounded by the slowest single target, not the count).

Documented in `internal/api/openapi.yaml` as a first-class endpoint
(request/response schema + example) - this is the primary way the user
intends to use this feature, not an implementation detail behind the bot.

### Bot UI

New third button on the main menu, a container for this and any future
one-off "special" checks:
```
[Markup.button.callback('➕ Дополнительные проверки', 'menu:extra')]
```
Submenu:
```
[Markup.button.callback('HTTP 101 check (Websocket)', 'extra:http101')]
[Markup.button.callback('◀️ Назад', 'menu:root')]
```

Flow (no router selection - always the `server` node, so this step is
skipped entirely, unlike ping/health):
1. Tap "HTTP 101 check (Websocket)" → bot sets
   `ctx.session.awaitingUpgradeScanTargets = true` and prompts for a
   target list (IPv4 / CIDR / range / domain, one per line or
   comma-separated).
2. Text handler reuses `parseTargetsMultiline` (already used for the
   Health Report custom list, `bot/src/index.ts`) unchanged. If the parsed
   (post-CIDR-expansion) list exceeds 100, reject with a clear message
   (target count, the 100 cap) and keep the session flag set so the user
   can just resend a narrower list - no silent truncation.
3. New `pingachock-client.ts` function `scanUpgrade(targets: string[]):
   Promise<{target: string; matched: boolean}[]>` calls
   `/api/v1/server-upgrade-scan` (`'api'` token, same as `ping`/`serverPing`).
4. Result rendering - icon only, no trailing label (explicitly requested -
   text like "— не апгрейдится" after every line was rejected as ugly):
   ```
   HTTP 101 check (Websocket)
   Время проверки: 11.08.2026, 15:42:07

   ✅ 185.229.226.146
   ❌ 94.140.14.14
   ❌ grokotun.ru

   Итог: 1 из 3 отвечают условию
   ```
   Same `formatHumanDate` used by every other report. Chunked via
   `chunkText` if the message would exceed Telegram's limit (up to 100
   lines can get long).

## Testing

- `internal/checks/upgrade_test.go`: a local `httptest`-style raw
  `net.Listener` standing in for a real server - one test where the
  handler writes back a genuine `101 Switching Protocols` response, one
  where it answers normally (400/200), one connection-refused case, one
  timeout case (server accepts but never responds, mirrors
  `TestTLSCheckerHandshakeDeadline`'s pattern in `tls_test.go`).
- `internal/api/public/serverupgradescan_test.go`: mirrors
  `serverping_test.go`'s structure - open/non-matching local listeners,
  target-count limit (empty and >100), concurrent-requests-don't-cross-talk
  (same regression shape as `TestServerPingConcurrentRequestsDoNotCrossTalk`).
- `bot/src/pingachock-client.ts`: `scanUpgrade`'s request/response mapping
  gets a unit test alongside the existing `formatIcmpSummary`/
  `classifyBlocked` ones; full end-to-end bot flow is exercised manually
  against the real backend (same as every other bot feature so far in this
  project).

## Next steps

Invoke `superpowers:writing-plans` to turn this into an implementation
plan.
