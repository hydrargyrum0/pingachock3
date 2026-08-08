# Ping result classification & error handling — design (WIP)

Status: **DONE. Both Section A and Section B are implemented.**

Section A: `classifyBlocked` in `bot/src/pingachock-client.ts`, 🚫 icon in
`bot/src/index.ts`; resolved-IP capture made unconditional in
`internal/checks/ping.go` and `internal/checks/tcp.go` - it used to only
populate when a custom per-node DNS resolver was configured, which is rare,
so `resolved_ip` was silently empty/useless almost all the time.

Section B item 2 (real latency): `averageReplyTimeMs` in
`internal/checks/ping.go` averages each reply's own RTT, anchored on
`TTL=` as proposed below, instead of falling back to wall-clock elapsed
time - plus a `windowsReplyTimeNoTTLRe` fallback for Windows IPv6 replies,
which never include a `TTL=` field at all (found during a follow-up review,
see below). Also added (not originally scoped here, but the user asked for
it directly, same conversation): packet-loss display ("3 из 4") via
`formatIcmpSummary` in `bot/src/pingachock-client.ts`, fed by
`packets_sent`/`packets_recv` now plumbed through `/api/v1/server-ping`'s
response too.

Section B item 1 (error normalization): `internal/checks` no longer emits
raw Go/exec error text. `classifyPingError`/`classifyNetError` map failures
to short, stable, English tokens (`"no reply"`, `"timeout"`, `"dns
resolution failed"`, `"connection refused"`, `"certificate verification
failed"`, ...) - this stays language-neutral since it's also the public
API's `ErrorMessage` contract (`internal/api/openapi.yaml`).
`bot/src/pingachock-client.ts`'s `translateCheckError` maps those tokens to
the Russian text bot users actually see, falling back to the raw token
(never blank) for anything not yet in its table.

A follow-up multi-agent code review of the six commits that implemented
Section A / B item 2 (before item 1 landed) also found and fixed several
correctness bugs introduced in that pass: a TLS check against a raw IP with
default params always failed (Go's crypto/tls refuses `ServerName: "",
InsecureSkipVerify: false`); the TLS handshake had no deadline of its own
and could hang a Runner slot forever; `requests_sent` misreported attempts
on early context cancellation; the virtual "server" node's provisioning had
a boot-time race that could crash-loop a second concurrently-starting
backend instance; DNS resolution used a hardcoded 5s timeout regardless of
the check's own `timeout_ms` and had no IPv4/family preference; and
`/api/v1/server-ping` had multiple goroutines independently re-resolving
the same target and racing to report `resolved_ip`. See git log for the
individual commits.

## Problem

Bot users see raw Go errors (`ICMP: exit status 1`) instead of a meaningful
verdict, and some domain-ping ICMP latencies are absurdly large (e.g.
`8233 ms`) because they're a wall-clock fallback, not real per-packet RTT.

## Section A — three-state classification (APPROVED)

For **domain targets only** (not raw IP targets):

- Domain resolves to `127.0.0.1` and `127.0.0.1` is pingable → **blocked**
  (DNS-level censorship signature specific to the Turkmen network).
- Domain resolves to something else, but that IP is unreachable → **unreachable**
  (real network failure, not censorship).
- Otherwise → **OK**.

For raw IP targets: no 127.0.0.1 special-casing. ICMP-filtered-but-TCP-open
is normal, non-censorship behavior for a bare IP and must not be
misclassified as "blocked."

## Section B — error normalization + real latency (IMPLEMENTED)

1. **Error message normalization**: `internal/checks` classifies failures
   into short, stable, English tokens instead of leaking raw Go/exec error
   text; `bot/src/pingachock-client.ts`'s `translateCheckError` renders
   them in Russian ("нет ответа" for `"no reply"`, etc.) - see the status
   note above for the full list.
2. **Real per-packet latency extraction**: locale-independent extraction
   anchored on `TTL=` in `internal/checks/ping.go` (`averageReplyTimeMs`),
   with a `windowsReplyTimeNoTTLRe` fallback for Windows IPv6 replies
   (which never print `TTL=` at all), replacing the wall-clock `elapsedMs`
   fallback.

## Next steps

None outstanding for this design. Both sections are implemented, tested,
and merged.
