# TLS handshake speed check + placeholder target — design

Status: APPROVED, ready for implementation planning.

## Problem

The user is evaluating candidate VPN relay/fronting servers. Setup: a relay
candidate (e.g. `123.123.123.123:443`) terminates TLS itself and, based on
the SNI it sees in the ClientHello, forwards the decrypted plaintext
connection to wherever that SNI is supposed to go - in this case, to
pingachock's own backend server on a dedicated port (`12.12.12.12:1343`).
This is the classic domain-fronting relay shape, same as
`internal/transport/fronted.go` already documents for the agent's own
transport.

The user wants to measure **only** how fast the TLS handshake against the
relay candidate completes (dial the relay's real IP, present a chosen SNI,
time the handshake) - not anything about the forwarded traffic itself. Two
things are needed:

1. Something listening on pingachock's own server for the relay to forward
   to (a placeholder - "no real function needed", explicitly no TLS on
   this side, since the relay already terminated TLS and forwards plain
   traffic).
2. A way to run the handshake-timing check itself, from a real node
   (**not** the "server" virtual node this time - the whole point is
   measuring from the node's network vantage point, e.g. Turkmenistan).

## Scope

**In scope:**
- A minimal placeholder WebSocket-upgrading TCP listener, port 1343
  (configurable via `PORT` env), no application logic beyond completing
  the WS opening handshake and holding the connection open. No domain, no
  TLS, no Caddy routing - this listener knows nothing about how it's
  reached; that's entirely the user's own external relay/DNS setup,
  outside this repo.
- Reusing the existing `internal/checks/tls.go` `Checker` (already
  implements exactly "dial IP:port, present a given SNI, measure handshake
  time only, `allow_insecure` to skip cert trust") via the existing
  generic `/api/v1/checks` + `node_selector` dispatch - no new Go check
  type needed.
- A new bot flow under "➕ Дополнительные проверки" ("TLS Handshake") that
  asks for a target IP + SNI, dispatches a `tls` check to a chosen node,
  and reports the handshake latency.

**Explicitly out of scope for this pass** (not deferred as TODO, just not
built now - can be added later as its own small change if wanted):
running the check against **all** online nodes simultaneously the way
`__all__` already works for ping/health elsewhere in the bot. This
feature's node picker supports `auto` (any one online, unblocked node) and
picking one specific node by name - matching the user's actual stated
scenario (testing one candidate relay from a chosen vantage point), not
the "ALL" fan-out several other bot flows have. Adding `__all__` later
would be a small, isolated addition (same `node_selector` mechanism,
already generic) and isn't blocked by anything here.

## Architecture

### Part 1: placeholder target (`cmd/handshaketarget`)

New Go binary, `cmd/handshaketarget/main.go`. Accepts an HTTP request,
validates it's a WebSocket upgrade (`Upgrade: websocket` +
`Sec-WebSocket-Key` present), hijacks the connection, computes
`Sec-WebSocket-Accept` (SHA-1 of the key + the RFC 6455 magic GUID,
base64), writes back a `101 Switching Protocols` response, then just reads
from the connection in a loop (discarding everything) until the peer
closes it. No message framing, no echo, no timers - "no real function" per
the user's own requirement. Listens on `:$PORT` (default `1343`), plain
HTTP/WS, no TLS - the relay in front of it already terminated TLS and
forwards plaintext.

New `Dockerfile.handshaketarget` at the repo root, same shape as the
existing root `Dockerfile` (build context `.`, so it can `COPY cmd/
internal/` and share `go.mod`), just building `./cmd/handshaketarget`
instead of `./cmd/server`.

New `docker-compose.prod.yml` service:
```yaml
handshake-target:
  build: { context: ., dockerfile: Dockerfile.handshaketarget }
  restart: unless-stopped
  ports:
    - "1343:1343"
```
Published to the host (not just the internal network) since whatever
terminates TLS for the user's chosen domain lives outside this compose
stack entirely and needs to reach this port from outside the container
network. No `depends_on`, no shared network needs - fully standalone,
doesn't touch postgres/backend/bot.

### Part 2: the check itself

No new Go check type - `internal/checks/tls.go`'s existing `TLSChecker`
already does exactly this: dial `target:port`, present `params.sni`,
measure handshake duration only, skip cert verification when
`allow_insecure` is set. Dispatched exactly like any other node-routed
check already in the bot (`ping`'s `nodePing()`/`createBatchedCheck()`/
`pollCheckUntilDone()` pattern in `bot/src/pingachock-client.ts`), just
with `type: "tls"` instead of `"ping"`/`"tcp"`, and `node_selector`
resolved to one specific real node (never `"server"` - there is no
server-side equivalent for this check, it only makes sense from a node's
network vantage point).

```
POST /api/v1/checks
{ "type": "tls", "targets": ["123.123.123.123"],
  "params": { "port": 443, "sni": "pingachock.com", "allow_insecure": true },
  "node_selector": { "node_ids": ["<resolved node id>"] } }
```
`allow_insecure: true` is fixed (not user-configurable) - the user only
cares about handshake speed, and the placeholder's own TLS termination
(done by the user's relay, not this repo) has no reason to present a
certificate trusted by the checking node.

### Part 3: bot UI

New entry under the existing "➕ Дополнительные проверки" submenu
(`bot/src/index.ts`, alongside "HTTP 101 check (Websocket)"):
```
[Markup.button.callback('TLS Handshake', 'extra:tlshandshake')]
```

Flow, mirroring ping's router-toggle-then-type pattern
(`getPingRouterOption`/`pingKeyboard` in `bot/src/index.ts`), with its own
independent session state so it can't cross-contaminate ping's:
1. Tap "TLS Handshake" → bot fetches online routers
   (`apiClient.listRouters()`, same as ping does), shows a keyboard with a
   router-toggle button (`Auto` / a specific router name, cycled by
   repeated taps - no `ALL`, see Scope) and a prompt: *"Выбери узел (кнопка
   выше) и пришли цель: IP и домен (SNI) через пробел, например:
   123.123.123.123 pingachock.com"*.
2. Text handler (new `awaitingTlsHandshakeTarget` session flag): splits the
   message on whitespace/comma, expects exactly two non-empty tokens.
   First token must be a literal IP (`net.isIP` - matches the user's own
   description, "запрос на 123.123.123.123:443", not a domain); second
   token is the SNI, any non-empty string (no format validation beyond
   that - it's just a hostname the user is asserting DNS/relay-config
   makes sense, pingachock has no way to check that from here).
3. Resolves the node via the same `resolveNodeId`-style logic `ping`
   already uses for `router_name` (`auto` → first online+unblocked;
   otherwise exact name match), dispatches the `tls` check, polls
   (`pollCheckUntilDone`, same 60s ceiling and reasoning as node-routed
   ping - see `NODE_POLL_TIMEOUT_MS`'s doc comment in
   `pingachock-client.ts`), reads back `latency_ms`/`success`/
   `error_message` from the one check_run.
4. Report:
   ```
   TLS Handshake Check
   Время проверки: 12.08.2026, 10:15:03
   Цель: 123.123.123.123:443, SNI: pingachock.com
   Узел: rebro

   ✅ 245 ms
   ```
   or, on failure, using the same `translateCheckError` Russian-token
   mapping the ICMP path already uses (Section B item 1 of
   `docs/superpowers/specs/2026-07-25-ping-result-classification-design.md`):
   ```
   ❌ ошибка: таймаут
   ```

## Testing

- `cmd/handshaketarget`: a small test dialing the listener, performing a
  real WS opening handshake by hand (send the upgrade request, assert the
  `101` + a well-formed `Sec-WebSocket-Accept` come back), and a second
  test asserting a non-upgrade HTTP request gets rejected (400), not
  silently hijacked.
- Bot: the pure parsing/mapping pieces (splitting "IP SNI" input, node
  resolution, result formatting) get unit tests alongside the existing
  ones in `pingachock-client.ts`, following the same pattern as
  `scanUpgrade`/`mapUpgradeScanResults`. The Telegraf wiring itself
  (`bot/src/index.ts`) is verified via `tsc --noEmit` + manual runthrough,
  same as every other menu flow in this bot (no unit test target there).
- No new Go check-type tests needed - `internal/checks/tls_test.go`
  already covers `TLSChecker` thoroughly (this design reuses it exactly
  as-is, no changes to that file).

## Deployment note

`handshake-target` is a new, independent compose service - deploying it
doesn't require restarting `backend`/`bot`/`postgres`/`caddy`. The user
still needs to (a) point their relay candidate's forwarding rule at
`<server-ip>:1343` and (b) configure the SNI-based routing/DNS on the
relay side - both explicitly outside this repo, per this design's Scope
section.

## Next steps

Invoke `superpowers:writing-plans` to turn this into an implementation
plan.
