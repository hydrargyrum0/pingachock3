# VLESS config speed test — design

Status: APPROVED, ready for implementation planning.

## Problem

Given a full xray-core VLESS config (JSON), actually stand up that tunnel
on a real measurement node and run a download speed test through it -
not just "is it reachable" (that's the separate TLS-handshake-speed check,
`docs/superpowers/specs/2026-08-12-tls-handshake-check-design.md`), but
"does this VLESS relay actually deliver usable throughput from inside the
censored network."

## Hard constraint (drives most of this design)

**Nodes cannot download anything at runtime.** Everything the check needs
- specifically the `xray-core` binary that actually speaks the VLESS
protocol - must be baked into the `pingachock-agent` executable itself at
build time (`//go:embed`), not fetched over the network when a check runs.
This was an explicit, deliberate requirement, not a default: the earlier
"agent downloads xray-core from GitHub on first use" option was rejected
specifically because GitHub isn't reachable through the same fronted
transport `internal/transport` already exists for (agent<->backend
traffic), and a censored node's first VLESS check would just hang/fail on
that download.

**Consequence, stated plainly:** `xray.exe` itself is **~35.6 MB**
uncompressed (checked against the real `v26.3.27` Windows release - not
the full ~20MB zip, which also bundles `geoip.dat`/`geosite.dat`/
`wintun.dll` that this checker doesn't need and won't embed). Embedding it
means every `pingachock-agent-*` binary grows from ~10 MB to **~45-50 MB**.
This is accepted as the cost of the "zero runtime downloads" requirement.

## Scope

**In scope:** the VLESS-config-in, speed-test-out check itself, embedded
xray-core, agent-only (matches the TLS handshake check's own reasoning -
this only means something from a real node's network vantage point, never
the "server" virtual node, though nothing in the code actively forbids
dispatching it there - the bot's UI simply never offers "server" as a
target for this check).

**Out of scope, not deferred as TODO:**
- Verifying xray-core binaries are unflagged by antivirus/Defender
  heuristics on Windows nodes - the user explicitly accepted this risk
  ("нормально, разберёмся по месту если возникнет").
- Solving GitHub reachability for xray-core downloads - moot now that
  nothing downloads at runtime, the fetch happens once at build time on a
  normal (uncensored) developer machine.
- Upload speed, jitter, or any metric beyond download throughput.
- Any UI for constructing a VLESS config from scratch - the bot only ever
  accepts a config the user already has (as the earlier design note said:
  a complete, ready-to-run xray-core `config.json`).

## Architecture

### Embedding xray-core into the agent

New build step, `scripts/fetch-xray.sh`: for each of the 6 platform/arch
combos `scripts/build-agent.sh` already targets, downloads the matching
`v26.3.27` release asset from `XTLS/Xray-core`'s GitHub releases,
verifies its SHA2-256 against the accompanying `.dgst` file (protects
against corrupted/incomplete downloads - not a full supply-chain defense,
since the checksum comes from the same source as the binary, but real
protection against the failure modes that actually happen in practice),
unzips just the `xray`/`xray.exe` binary (nothing else from the release
archive), and places it at
`internal/checks/embedded/<goos>_<goarch>/xray[.exe]`.

| GOOS/GOARCH | Release asset |
|---|---|
| windows/amd64 | `Xray-windows-64.zip` |
| windows/386 | `Xray-windows-32.zip` |
| linux/amd64 | `Xray-linux-64.zip` |
| linux/arm64 | `Xray-linux-arm64-v8a.zip` |
| darwin/amd64 | `Xray-macos-64.zip` |
| darwin/arm64 | `Xray-macos-arm64-v8a.zip` |

`internal/checks/embedded/` is `.gitignore`d - these are large,
third-party, mechanically-fetched binaries, not source; they don't belong
in git history. `scripts/build-agent.sh` calls `fetch-xray.sh` once up
front (before its existing per-target build loop) so every subsequent
`go build` in that loop already has the files it needs to embed.

Six new tiny Go files in `internal/checks/`, each gated by a build tag
matching exactly one platform combo, each `//go:embed`-ing its own
platform's binary into a common exported symbol:

```go
//go:build windows && amd64

package checks

import _ "embed"

//go:embed embedded/windows_amd64/xray.exe
var embeddedXrayBinary []byte

const embeddedXrayFilename = "xray.exe"
```

(and five siblings: `xray_windows_386.go`, `xray_linux_amd64.go`,
`xray_linux_arm64.go`, `xray_darwin_amd64.go`, `xray_darwin_arm64.go` -
each with its own `//go:build` line, its own `//go:embed` path, and
`embeddedXrayFilename = "xray"` for the three non-Windows ones). Go's
build-tag exclusion means only the one file matching the actual
`GOOS`/`GOARCH` being compiled is ever part of the build, so
cross-compiling for one platform never needs the other five platforms'
binaries to exist locally.

**`cmd/server`'s Docker image also embeds one binary** (linux/amd64,
matching its own runtime) - it imports `internal/checks` too (for
`internal/serveragent` to run any check type against the virtual "server"
node), so it needs `fetch-xray.sh`'s linux/amd64 step run before its own
`go build`. This grows the backend image by the same ~35 MB. `Dockerfile`
gets a `RUN` step invoking a slimmed single-platform version of the fetch
before `go build ./cmd/server`.

At runtime, `ensureXrayBinary()` writes `embeddedXrayBinary` out to a
per-OS user cache directory (`os.UserCacheDir()` + `pingachock-agent/xray/
<embeddedXrayFilename>` - resolves correctly whether the agent runs as a
normal user process or as a Windows Service/systemd unit under a service
account) once per process lifetime, `chmod 0o755` on non-Windows, and
reuses that path on every subsequent check without re-writing it (checked
via a marker/size comparison, not re-extracted every single check run -
avoids needless disk I/O on a node that might run many VLESS checks).

### The check: `internal/checks/vless.go`

New `Checker` type `"vless"`, registered in the same registry as
`ping`/`tcp`/`tls`/`upgrade`. Unlike every other checker, the *target*
here is just a free-text label (the config itself doesn't fit naturally
into a single `target` string) - the real payload travels in `params`:

```go
type vlessParams struct {
    Config json.RawMessage `json:"config"` // a full xray-core config.json
}
```

Steps:
1. Unmarshal `params.Config` - if missing/empty, fail immediately with a
   clear "config required" error, no subprocess involved.
2. `patchInbound(config)`: parse just enough of the JSON to replace the
   top-level `"inbounds"` array with exactly one fixed entry - a SOCKS5
   listener bound to `127.0.0.1` - and leave everything else
   (`outbounds`, `routing`, ...) untouched. This is deliberate, not a
   merge: whatever inbound the user's config specified (if any) is
   irrelevant and is never applied - the agent fully controls the local
   listening side, so it's guaranteed to never be reachable from outside
   the node. The port is picked fresh for *this* invocation (bind a
   throwaway `net.Listener` on `127.0.0.1:0`, read back the OS-assigned
   port, close it, hand that port number to xray) rather than a single
   hardcoded constant - a node can run several check_runs concurrently
   (`MaxConcurrent`, same as every other checker), and two simultaneous
   VLESS checks both trying to bind the same fixed port would collide.
3. Write the patched config to a temp file, `exec.CommandContext(ctx,
   xrayPath, "run", "-c", tempConfigPath)`, capturing stderr.
4. Poll-dial `127.0.0.1:<socks port>` (short retry loop, up to 5s) until
   xray's SOCKS listener accepts connections - if it never does within
   that budget, kill the process and report a config/startup error built
   from xray's own stderr (trimmed/cleaned, not a raw dump - same spirit
   as `classifyNetError`, but this one genuinely benefits from surfacing
   *some* of xray's own message, since a bad VLESS config is an
   operator-actionable problem, unlike a generic network timeout).
5. Speed test: an `http.Client` whose `Transport.DialContext` goes through
   the local SOCKS5 proxy (`golang.org/x/net/proxy` - new, small,
   standard dependency), `GET
   https://speed.cloudflare.com/__down?bytes=10000000` (fixed endpoint,
   **not** taken from the request - letting the caller choose an
   arbitrary download URL would turn measurement nodes into a way to
   generate traffic to/download from anywhere, which is exactly the kind
   of open-relay risk this design avoids on purpose). Reads the response
   body counting bytes, stopping at whichever comes first: 10 MB
   downloaded, or 15 seconds elapsed. Computes `mbps = bytes*8 /
   (elapsed_seconds * 1_000_000)`.
6. Kill the xray subprocess (`defer`), clean up the temp config file.

Result shape: `Success` = the whole pipeline (config valid, tunnel up,
speed test completed) worked; `Raw` = `{"mbps": <float>,
"bytes_downloaded": <int>, "duration_ms": <int>}`; `ErrorMessage` (via the
existing `classifyNetError`/xray-stderr-derived message) on any failure
stage, with `Raw` still including whatever partial info is available
(e.g. `bytes_downloaded` even on a timeout mid-download).

No changes needed to `internal/checks/checks.go` beyond one registry line
- this integrates through the exact same `/api/v1/checks` +
`node_selector` dispatch every other node-routed check already uses,
same as the TLS handshake check design.

### Bot UI

Fourth entry under "➕ Дополнительные проверки":
```
[Markup.button.callback('VLESS Speedtest', 'extra:vlessspeedtest')]
```
Flow, same router-toggle-then-type shape as the TLS Handshake check (own
independent session state, `auto` or one specific node, no `ALL` - same
reasoning as that design): prompts for the xray config JSON, pasted as a
message (or as an uploaded `.json` document, since a real config can be a
few KB and awkward to paste inline in Telegram - the text handler checks
`ctx.message.document` first, falls back to `ctx.message.text`).
Validates it's parseable JSON before dispatching (reject with a clear
message otherwise - don't waste a node's time on nodes on already-invalid
JSON). Reports success + Mbps, or the classified failure reason.

## Testing

- `internal/checks/vless_test.go`: `patchInbound` is pure and fully unit
  testable (given a config with/without an existing `inbounds` key,
  assert the result always has exactly one SOCKS entry on the chosen
  port, everything else preserved byte-for-byte where unrelated).
  `ensureXrayBinary`'s caching logic (write-once, reuse-after) is testable
  with a fake small byte slice standing in for the real embedded binary,
  not the real 35MB one. A full end-to-end test (real xray subprocess,
  real VLESS server, real speed test) is **not** part of the automated
  suite - it needs a real VLESS server to connect to, which is
  infrastructure this repo doesn't stand up for tests; verified manually
  once implemented, same as this project's established practice for
  anything needing live external infrastructure
  (`pingachock-client.test.ts`'s own documented live-backend requirement
  is the precedent).
- `scripts/fetch-xray.sh` gets a manual verification run (does it fetch,
  checksum-verify, and place the binary correctly) rather than an
  automated test - it's a build-time shell script hitting a real external
  service (GitHub releases), not application logic.

## Next steps

Invoke `superpowers:writing-plans` to turn this into an implementation
plan.
