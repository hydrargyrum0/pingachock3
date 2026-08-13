# Node networking resilience against a locally-running VPN/proxy — design

Status: APPROVED, ready for implementation planning.

## Problem

A node is an ordinary, everyday-use computer — the operator runs it
alongside whatever else they normally run, which very often (per the
user, roughly 80% of the time) includes a VPN client or an XRAY/V2Ray-style
proxy client. The user reported a real node producing "very many fake
results, across all check types" during a session where this was active.

The agent already has *some* defense against this: `internal/netiface`
lets the operator pin checks to a specific physical network interface (as
opposed to whatever virtual/VPN adapter the OS might currently prefer),
and most checkers (ping/tcp/http/dns/tls/upgrade) already take that pinned
interface's address as their dial source. Investigating why that wasn't
enough surfaced four separate, independent gaps:

1. **The pinned address goes stale.** `buildNetConfig` (cmd/agent/main.go)
   reads the operator-chosen interface's IP address *once*, at agent
   startup, and keeps using that exact string for the life of the process.
   If that address changes — DHCP lease renewal, Wi-Fi reconnect,
   sleep/wake, switching networks — every checker's `net.Dialer.LocalAddr`
   is now binding to an address the OS no longer considers local, and the
   bind fails outright. Because this affects every checker's dial
   uniformly and simultaneously, this is the most likely explanation for
   "fake results in every check type at once."
2. **`VLESSChecker` doesn't use `NetConfig` at all.** Every other checker
   takes `netCfg NetConfig` and threads it into its dialer; VLESSChecker's
   `Run` accepts the parameter (interface compliance) but never reads it.
   The embedded xray-core subprocess it spawns dials out over whatever the
   OS's current default route is — if a VPN has claimed that route, VLESS
   Speedtest silently measures the VPN's throughput, not the node's own.
3. **The physical/VPN distinction is checked once, at `configure` time.**
   `chooseInterface` warns the operator if they explicitly pick a
   non-physical adapter, but nothing re-checks this later — a VPN that
   starts *after* configuration, or a route change that happens later,
   goes unnoticed.
4. **A minority of VPN/proxy setups can't be defeated by interface pinning
   at all.** Most VPN clients just adjust routing metrics for *unbound*
   sockets — explicit interface binding (see below) genuinely routes
   around them. But some (system-wide "killswitch" modes, some XRAY-TUN
   configurations) intercept at a lower OS layer (WFP callouts on
   Windows) that doesn't care what interface a socket asked to bind to.
   This is a real technical wall, not a configuration gap — no
   userspace socket option defeats a kernel-level packet filter designed
   specifically to prevent exactly this kind of bypass.

## Goals

- For the common case (gaps 1–3): actually **prevent** the interference,
  not detect-and-annotate it. A correctly node next to an always-on VPN
  should produce the same results it would produce with no VPN running at
  all.
- For the uncommon case (gap 4, true killswitch-level interception):
  accept that prevention is impossible, but guarantee the agent never
  reports a *wrong* result as if it were a real measurement — a missing
  data point is acceptable, a fabricated one is not. This has to run as a
  background safety net that doesn't stall or pause the agent's normal
  operation, and has to recover on its own the moment the interference
  clears — no operator intervention, no restart.
- No behavior change for a node with no interface configured (`netCfg`
  zero value) — everything here only activates once an operator has
  pinned a physical interface via `configure`, exactly like today.

## Non-goals

- Defeating true killswitch-level interception (gap 4) is explicitly
  **not** attempted — see Goals above, the answer there is "don't report
  bad data," not "find a way through."
- No changes to how the **backend connection itself** is dialed — that
  already deliberately ignores the pinned interface (see
  `docs/ARCHITECTURE.md`) so the agent can always reach the backend to
  report status/pull commands regardless of the checks-interface's state.
  Out of scope here, unaffected either way.
- No UI/bot changes. Everything here is agent-internal; the only
  user-visible effect is that results become accurate instead of wrong
  (or a check-run comes back with a distinct, existing-shaped error like
  any other check-run failure, if the pinned interface is genuinely gone).

## Architecture

### Part 1: bind by interface identity, not a snapshot address

Root cause of gap 1 is `NetConfig.LocalAddr` being a `net.IP` frozen at
startup. The fix isn't "refresh it more often" — it's binding sockets to
the interface itself and letting the OS resolve its current address at
connection time, the same primitive VPN clients themselves use to claim
an adapter:

- Windows: `IP_UNICAST_IF` (an `IPPROTO_IP`-level socket option taking an
  interface index, set via `setsockopt`)
- Linux: `SO_BINDTODEVICE` (interface name)
- macOS: `IP_BOUND_IF` (interface index)

All three are set via a `net.Dialer.Control` callback (`func(network,
address string, c syscall.RawConn) error`), which every checker's dialer
already has a natural place to receive without changing its own
signature — this becomes something `NetConfig` carries alongside (or
instead of) `LocalAddr`.

`internal/netiface` grows a per-OS `BindToInterface(name string)
func(network, address string, c syscall.RawConn) error` (or equivalent),
mirroring the existing per-OS `isPhysical` split
(physical_{windows,linux,darwin}.go). `config.Config.InterfaceName`
already exists and is already persisted by `configure` today — it's
simply not read by `buildNetConfig` yet. `buildNetConfig` starts reading
it and building this Control-based binding instead of (in addition to,
for the address-family-preference logic that still needs *some* concrete
address — see `pickPreferredIP`) resolving a fixed `LocalAddr` once.

The custom DNS resolver `buildNetConfig` already builds (dialing the
node's own chosen DNS server rather than the system resolver) gets the
same Control-based binding on its own dial — it has exactly the same
staleness exposure today.

**Old agent configs with `interface_name` empty but `local_addr` set**
(pre-existing installs from before this change): fall back to resolving
which currently-listed interface owns that address at startup, same as
today, and keep working — not a hard migration, just a fallback path.
No config file format change, no migration needed.

### Part 2: `VLESSChecker` gets `NetConfig` too

xray-core's outbound config already has exactly the field this needs:
`sendThrough`, a per-outbound local IP to bind egress connections to. VLESS
Speedtest's `patchInbound` (already the place that force-overrides
`inbounds` and `log` for every caller-supplied config, see the previous
session's fix) grows one more override: when `NetConfig` carries a pinned
interface, resolve that interface's *current* address (via
`internal/netiface`, fresh at check-run time — never a cached string, for
the same staleness reason as Part 1) and set `sendThrough` on every
outbound that actually performs network egress (i.e., everything except
`blackhole`).

`xray-core` doesn't expose an interface-identity bind option in its
config schema (only a literal IP via `sendThrough`), so this specific
piece can't use Part 1's Control-callback trick — resolving the pinned
interface's address fresh, right before building the config, is the best
available equivalent (closes the staleness gap the same way in practice:
the address is looked up at the moment it's used, never trusted from a
snapshot taken earlier).

### Part 3: pinned interface disappearing entirely

Binding to an interface identity that no longer exists (unplugged cable,
Wi-Fi off, laptop closed) fails at dial time with a clear OS-level error
(no such device / interface down), distinct from any target-side failure.
`classifyNetError` (internal/checks/checks.go) gains a case for this —
a new stable token (e.g. `"network interface unavailable"`) instead of it
falling into the generic `"connection failed"` bucket, so the bot can
show the operator something actionable ("agent's pinned interface is
gone, needs re-`configure`") instead of it looking like every target on
earth suddenly went down. Per the earlier discussion: **no automatic
fallback to a different interface** — silently switching (possibly onto
a VPN adapter, if that's all that's left) is exactly the failure mode
this whole design exists to prevent.

### Part 4: background path-integrity self-test (the gap 4 safety net)

A new lightweight, independent loop (own ticker, e.g. every 2–3 minutes,
not tied to or blocking the main poll/execute cycle in
`internal/poller`) that:

1. Dials a couple of highly-reliable, always-up global targets (e.g.
   1.1.1.1:443, 8.8.8.8:443) **twice per target** — once through the
   pinned-interface binding (Part 1), once completely unbound (default
   route).
2. If the bound attempt fails while the unbound one succeeds, that's the
   signal: something is intercepting traffic that explicitly asked to
   leave through a specific interface, i.e. exactly gap 4. (This is
   deliberately a *differential* test, not "is 1.1.1.1 reachable" against
   some assumed-always-true baseline — with no VPN active, bound and
   unbound are the same physical path and always agree, so this can never
   false-positive just because Turkmenistan is having a bad day with
   Cloudflare; it only fires when the two paths actually diverge.)
3. Result is a simple in-memory flag (`pathSuspect bool`, with a
   timestamp), read by the poller's result-submission step: while set,
   this tick's job results are executed exactly as normal (checks aren't
   paused or delayed) but are **not** posted to the backend as real
   measurements — either withheld entirely for that tick, or submitted
   tagged with a distinct status the backend already has room for
   (`check_run` failure states), whichever turns out cleaner during
   implementation; the two are equivalent from "no fabricated data reaches
   the operator" standpoint.
4. The very next self-test tick (independent, still running on its own
   cadence in the background) clears the flag automatically the moment
   the interference stops — no restart, no manual step.

This only ever *withholds*, never blocks — the main poll loop keeps
running at its normal interval throughout, completely unaware of the
self-test except for reading the one flag right before submission.

## Data flow

```
configure (existing, unchanged)
  -> persists InterfaceName (already does) + LocalAddr + DNSServers

agent startup
  -> buildNetConfig reads InterfaceName
  -> NetConfig{ Bind: Control-callback for InterfaceName, Resolver: ... }
     (LocalAddr kept only where still needed for address-family choice)

background self-test loop (new, own ticker)
  -> every few minutes: bound-vs-unbound dial to 1.1.1.1/8.8.8.8
  -> updates pathSuspect flag

poller.tick (existing loop, unchanged cadence)
  -> executes jobs through NetConfig-bound dialers as today
  -> before PostResults: if pathSuspect, withhold/mark this batch
  -> otherwise: submit exactly as today
```

## Error handling

- Interface-identity bind failing because the interface is genuinely gone
  → new stable classification token, distinct from target-reachability
  failures (Part 3).
- Interface-identity bind succeeding but the *destination* being
  unreachable → completely unchanged, same classification as today (this
  design changes nothing about how a real target failure is reported).
- Path-integrity self-test itself erroring (e.g. its own dial times out
  for unrelated reasons on both bound and unbound) → treated as
  inconclusive, not a positive detection — only an explicit bound-fails
  while-unbound-succeeds asymmetry sets `pathSuspect`.

## Testing

- `internal/netiface`: unit tests for the new per-OS bind helper are
  necessarily thin (they're thin syscall wrappers, same as `isPhysical`
  already is) — the meaningful test is that the returned `Control` func
  has the right shape and doesn't error out when constructed; real
  syscall behavior isn't unit-testable without a real second interface
  present, consistent with how this package is tested today.
- `internal/checks`: `classifyNetError`'s new token gets a table-test
  entry the same way its existing tokens do.
- `internal/checks/vless.go`: `patchInbound`'s new `sendThrough`
  injection is deterministic pure-JSON-transform logic — same testing
  shape as the existing `TestPatchInboundAddsSocksWhenNoneExists` /
  `TestPatchInboundOverridesLogConfig` tests, easy to unit test fully
  (given a fake resolved IP, assert every non-blackhole outbound gets
  `sendThrough` set to it).
- The self-test's *decision* logic (bound-result + unbound-result →
  suspect or not) is pulled into its own pure function, unit-tested with
  all four success/failure combinations — the actual dialing is a thin,
  untested wrapper, consistent with how `internal/checks/ping.go`
  already separates `parsePingOutput`/`classifyPingError` (tested) from
  the `exec.Command` plumbing (not separately tested).
- No existing test should need to change behavior — everything here is
  additive to `NetConfig`'s zero-value (unconfigured) path, which stays
  exactly as it is today.

## Open questions for the implementation plan

- Exact shape of "withhold vs. tag" for Part 4 (see Part 4 point 3) —
  whichever is less invasive to `internal/poller`/`transport.Transport`'s
  existing submission path, decided during planning once both are looked
  at side by side.
- Whether the self-test's two target IPs should be hardcoded or a short
  configurable list — leaning hardcoded (1.1.1.1 + 8.8.8.8, both
  operationally about as reliable as global infrastructure gets) unless
  planning turns up a reason to make it configurable.
