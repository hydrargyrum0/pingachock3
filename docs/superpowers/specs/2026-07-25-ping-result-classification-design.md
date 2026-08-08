# Ping result classification & error handling — design (WIP)

Status: **Section A implemented** (`classifyBlocked` in `bot/src/pingachock-client.ts`,
🚫 icon in `bot/src/index.ts`; resolved-IP capture made unconditional in
`internal/checks/ping.go` and `internal/checks/tcp.go` - it used to only
populate when a custom per-node DNS resolver was configured, which is rare,
so `resolved_ip` was silently empty/useless almost all the time). **Section
B item 2 (real latency) implemented** - `averageReplyTimeMs` in
`internal/checks/ping.go` now averages each reply's own RTT, anchored on
`TTL=` exactly as proposed below, instead of falling back to wall-clock
elapsed time. Also added (not originally scoped here, but the user asked
for it directly, same conversation): packet-loss display ("3 из 4") via
`formatIcmpSummary` in `bot/src/pingachock-client.ts`, fed by
`packets_sent`/`packets_recv` now plumbed through `/api/v1/server-ping`'s
response too. **Section B item 1 (error message normalization) still not
confirmed** - raw Go error strings (`exit status 1` etc.) are unchanged.

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

## Section B — error normalization + real latency (NOT YET CONFIRMED)

Proposed, pending user sign-off:

1. **Error message normalization**: stop surfacing raw Go error strings
   (`exit status 1` etc.) to bot users. Map internal check failures to a
   short human-readable message (something like "нет ответа" /
   "недоступен") instead of leaking the underlying Go/exec error text.
2. **Real per-packet latency extraction**: the `ICMP: Nms` value bot users
   see is currently the wall-clock time the whole check took
   (`elapsedMs` fallback), not a real per-packet RTT — hence the
   implausible multi-second values. Replace it with a locale-independent
   extraction anchored on `TTL=` in `internal/checks/ping.go` (same anchor
   already used for locale-independent received-packet counting, see
   `parsePingOutput`), pulling the actual reported round-trip time instead
   of falling back to wall-clock elapsed time.

## Next steps

- Get explicit confirmation on Section B (or revisions) from the user.
- Once both sections are confirmed: finish the brainstorming spec per
  `superpowers:brainstorming`, self-review, get sign-off on the written
  spec, then invoke `superpowers:writing-plans`.
- Touches: `bot/src/pingachock-client.ts` (classification + error
  normalization) and `internal/checks/ping.go` (real-latency extraction).
