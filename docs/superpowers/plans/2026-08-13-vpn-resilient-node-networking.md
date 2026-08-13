# VPN-resilient node networking Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make node checks (and the VLESS speedtest) produce correct results when a VPN/proxy is running on the same machine, by binding to the operator-pinned physical network interface itself (not a cached snapshot address), and add a non-blocking background safety net for the rare case that can't be defeated this way.

**Architecture:** Replace `checks.NetConfig.LocalAddr`-based socket binding with a `net.Dialer.Control` callback that forces egress through a named interface (`IP_UNICAST_IF` on Windows, `SO_BINDTODEVICE` on Linux, `IP_BOUND_IF` on macOS), re-verified fresh on every single dial so neither a changed DHCP address nor a removed interface needs any cache invalidation. `PingChecker` (shells to the OS `ping` binary) and `VLESSChecker` (needs a literal IP for xray-core's `sendThrough`) can't use a Go-level `Control` callback, so they resolve the pinned interface's current address fresh on every run instead. A new background `PathSelfTest` in `internal/poller` differentially dials a couple of reliable global targets both bound and unbound every ~2 minutes; when the bound path fails while the unbound one succeeds, that's the signature of interception that binding can't defeat (packet-filter-level capture), and the poller withholds that tick's results from the backend rather than ever submitting a fabricated measurement.

**Tech Stack:** Go 1.26, `golang.org/x/sys/windows` + `golang.org/x/sys/unix` (already a direct dependency, no `go.mod` change needed), `net.Dialer.Control`/`syscall.RawConn`.

**Design doc:** `docs/superpowers/specs/2026-08-13-vpn-resilient-node-networking-design.md`

---

## Before you start

Read `docs/superpowers/specs/2026-08-13-vpn-resilient-node-networking-design.md` in full - this plan implements it exactly, including its four numbered gaps and the Part 1-4 architecture. A few things resolved during planning that go beyond what the design doc spells out (useful context, not a deviation from it):

- `config.Config.InterfaceName` (`internal/config/config.go:28`) already exists and is already populated by `configure` (`cmd/agent/main.go:943`) - no config file migration needed anywhere in this plan.
- `net.Dialer.Control`'s signature (`func(network, address string, c syscall.RawConn) error`) is what actually carries the interface-identity binding into every Go-level dialer - it doesn't need `LocalAddr` set at all once `Control` is; `IP_UNICAST_IF`/`SO_BINDTODEVICE`/`IP_BOUND_IF` all make the OS itself pick the correct source address for that interface.
- `PingChecker` shells out to the OS's own `ping` binary (not `net.Dialer`), and `VLESSChecker` writes a JSON config for a separate `xray-core` process to read - neither can take a `Control` callback. Both instead resolve the pinned interface's *current* address fresh on every `Run()` call via a new `internal/netiface.ByName`, which is exactly as immune to DHCP staleness as the `Control` approach, just via a different mechanism forced by each one's own constraints.
- `checks.NetConfig.LocalAddr` is kept, but repurposed: after this plan, it's used *only* as an address-family preference hint for `resolveIP`/`pickPreferredIP` (deciding whether to pick an IPv4 or IPv6 answer when a domain resolves to both) - never for actual socket binding. It staying resolved-once-at-startup is harmless for that narrower purpose, since a physical interface's address *family* essentially never changes even when its specific address does.

---

## Task 1: `internal/netiface` — `ErrInterfaceUnavailable` and `ByName`

**Files:**
- Modify: `internal/netiface/netiface.go`
- Test: `internal/netiface/netiface_test.go`

This task refactors `List()`'s per-interface conversion logic into a reusable `fromNetInterface` function, and adds `ByName` - a fresh, uncached lookup of one named interface's current state, for the checkers that need a literal current address (`PingChecker`, `VLESSChecker`) rather than a `Control` callback.

- [ ] **Step 1: Write the failing test**

Append to `internal/netiface/netiface_test.go`:

```go
func TestByNameReturnsCurrentState(t *testing.T) {
	ifaces, err := List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(ifaces) == 0 {
		t.Skip("no usable network interfaces in this environment")
	}
	want := ifaces[0]

	got, err := ByName(want.Name)
	if err != nil {
		t.Fatalf("ByName(%q) error = %v", want.Name, err)
	}
	if got.Name != want.Name {
		t.Errorf("ByName(%q).Name = %q, want %q", want.Name, got.Name, want.Name)
	}
	if len(got.Addrs) == 0 {
		t.Errorf("ByName(%q).Addrs is empty, want at least one address", want.Name)
	}
}

func TestByNameUnknownInterfaceReturnsErrInterfaceUnavailable(t *testing.T) {
	_, err := ByName("this-interface-does-not-exist-pingachock-test")
	if !errors.Is(err, ErrInterfaceUnavailable) {
		t.Errorf("ByName() error = %v, want errors.Is(err, ErrInterfaceUnavailable)", err)
	}
}
```

Add `"errors"` to the existing `import ( "net" "testing" )` block at the top of the file (alphabetical: `errors`, then `net`, then `testing`).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/netiface/... -run 'TestByName' -v`
Expected: FAIL - `undefined: ByName` and `undefined: ErrInterfaceUnavailable` (compile error).

- [ ] **Step 3: Write the implementation**

Replace the whole of `internal/netiface/netiface.go` with:

```go
// Package netiface lets the operator pick which network interface the agent
// should run checks through, and discovers that interface's own DNS servers
// - deliberately not the system-wide resolver, which can be silently
// overridden by a VPN client. It also flags which interfaces are backed by
// real hardware (IsPhysical) versus a VPN/tunnel/virtual adapter, since
// running checks through a VPN would measure what the VPN's exit node sees,
// not what the local ISP actually does - the whole point of this agent. See
// docs/ARCHITECTURE.md and the `configure` command in cmd/agent.
package netiface

import (
	"errors"
	"fmt"
	"net"
)

// ErrInterfaceUnavailable wraps every error this package returns when a
// pinned interface no longer exists (removed adapter, cable unplugged,
// Wi-Fi turned off) - checkers unwrap it with errors.Is to report a
// distinct, actionable classification instead of it looking like every
// check target on earth suddenly went down. See
// docs/superpowers/specs/2026-08-13-vpn-resilient-node-networking-design.md
// Part 3.
var ErrInterfaceUnavailable = errors.New("network interface unavailable")

type Interface struct {
	Name       string
	Addrs      []net.IP
	IsUp       bool
	IsPhysical bool
}

// PreferredAddr picks which of the interface's addresses to hand checks as
// their source address. Prefers IPv4: net.Dialer.LocalAddr (and ping's -S)
// must share an address family with the destination or the dial/ping fails
// outright before a single packet goes out, and check targets are
// overwhelmingly IPv4 - so an IPv6 address picked here would silently break
// every IPv4 check. Falls back to the first address (typically IPv6) if the
// interface has no IPv4 address at all.
func (i Interface) PreferredAddr() net.IP {
	if len(i.Addrs) == 0 {
		return nil
	}
	for _, a := range i.Addrs {
		if a.To4() != nil {
			return a
		}
	}
	return i.Addrs[0]
}

// List returns non-loopback interfaces that have at least one non-link-local
// address, since those are the only ones useful to route checks through.
func List() ([]Interface, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}

	var out []Interface
	for _, ifc := range ifaces {
		converted, ok := fromNetInterface(ifc)
		if !ok {
			continue
		}
		out = append(out, converted)
	}
	return out, nil
}

// ByName looks up a single interface's *current* state fresh, every call -
// unlike List, which is for presenting the whole choice to an operator at
// `configure` time, this is for checkers that need this interface's
// present-moment address right before using it (PingChecker, shelling out
// to the OS ping binary; VLESSChecker, building an xray-core config's
// sendThrough field) because they can't use netiface's Control-based
// BindControl the way every other checker does. Callers must call this
// fresh on every use, never cache the result across check runs, or they
// reintroduce exactly the staleness bug this whole package exists to fix.
// See docs/superpowers/specs/2026-08-13-vpn-resilient-node-networking-design.md.
func ByName(name string) (Interface, error) {
	ifc, err := net.InterfaceByName(name)
	if err != nil {
		return Interface{}, fmt.Errorf("%w: %s: %v", ErrInterfaceUnavailable, name, err)
	}
	converted, ok := fromNetInterface(*ifc)
	if !ok {
		return Interface{}, fmt.Errorf("%w: %s: interface has no usable address", ErrInterfaceUnavailable, name)
	}
	return converted, nil
}

// fromNetInterface converts a stdlib net.Interface into our Interface,
// applying the same filtering List has always applied (skip loopback, skip
// interfaces whose only addresses are link-local) - ok is false when ifc
// should be skipped entirely.
func fromNetInterface(ifc net.Interface) (Interface, bool) {
	if ifc.Flags&net.FlagLoopback != 0 {
		return Interface{}, false
	}
	addrs, err := ifc.Addrs()
	if err != nil {
		return Interface{}, false
	}
	var ips []net.IP
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip == nil || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			continue
		}
		ips = append(ips, ip)
	}
	if len(ips) == 0 {
		return Interface{}, false
	}
	return Interface{
		Name:       ifc.Name,
		Addrs:      ips,
		IsUp:       ifc.Flags&net.FlagUp != 0,
		IsPhysical: isPhysical(ifc.Name),
	}, true
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/netiface/... -v`
Expected: PASS - all tests, including the pre-existing `TestInterfacePreferredAddr` (unaffected by this refactor - `List()`'s observable behavior is unchanged).

- [ ] **Step 5: Commit**

```bash
git add internal/netiface/netiface.go internal/netiface/netiface_test.go
git commit -m "netiface: add ErrInterfaceUnavailable and ByName for fresh, uncached interface lookups"
```

---

## Task 2: `internal/netiface` — per-OS `BindControl`

**Files:**
- Create: `internal/netiface/bind_windows.go`
- Create: `internal/netiface/bind_linux.go`
- Create: `internal/netiface/bind_darwin.go`

`BindControl(name string)` returns a `net.Dialer.Control`-shaped callback that forces every dial through it to leave via the named interface - the actual fix for Part 1 of the design doc. No error return: it can't fail at construction time (it does nothing but capture `name` in a closure), only at dial time, when the interface it names might no longer exist.

This is unit-testable only in the thinnest possible way (real interface-binding behavior needs a real second interface to prove anything meaningful, which isn't available in a CI/dev sandbox) - the test that matters here is that it builds and links correctly on every target OS, checked in Task 12's cross-compile step. Mirrors how `isPhysical` (`internal/netiface/physical_*.go`) already has no dedicated unit tests of its own for the same reason.

- [ ] **Step 1: Write `bind_windows.go`**

Create `internal/netiface/bind_windows.go`:

```go
//go:build windows

package netiface

import (
	"encoding/binary"
	"fmt"
	"net"
	"syscall"

	"golang.org/x/sys/windows"
)

// ipUnicastIF is IP_UNICAST_IF (ws2ipdef.h, value 31) - not predefined in
// golang.org/x/sys/windows, so declared here. Forces IPv4 unicast egress
// through a specific interface regardless of what the routing table would
// otherwise prefer - the same mechanism VPN clients rely on to claim the
// default route, used here in reverse so a check's traffic leaves through
// the operator-pinned physical adapter instead of whatever a VPN/tunnel
// currently prefers. IPv6 has its own IPV6_UNICAST_IF option under
// IPPROTO_IPV6; not implemented here, matching this codebase's existing
// IPv4-first stance elsewhere (see internal/checks.pickPreferredIP) - an
// IPv6 dial through a pinned interface falls back to whatever the OS
// default route does for IPv6, unchanged from before this package existed.
const ipUnicastIF = 31

// BindControl returns a net.Dialer.Control callback that forces every dial
// through it to leave via the named interface. The interface is resolved
// fresh on every single call (never a cached index) - both so a
// DHCP-renewed address never matters (the index survives a renewal; only
// resolving it fresh matters for the case the interface itself is gone -
// see net.InterfaceByName's error below) and so a removed interface fails
// loudly and immediately with ErrInterfaceUnavailable, rather than
// silently falling back to whatever the OS would otherwise pick. See
// docs/superpowers/specs/2026-08-13-vpn-resilient-node-networking-design.md.
func BindControl(name string) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, c syscall.RawConn) error {
		ifc, err := net.InterfaceByName(name)
		if err != nil {
			return fmt.Errorf("%w: %s: %v", ErrInterfaceUnavailable, name, err)
		}

		// IP_UNICAST_IF is one of the few Windows socket options that wants
		// its DWORD value in network byte order, not host byte order - a
		// well-documented, easy-to-get-wrong quirk specific to this option.
		be := make([]byte, 4)
		binary.BigEndian.PutUint32(be, uint32(ifc.Index))
		indexNetworkOrder := int(binary.LittleEndian.Uint32(be))

		var sockErr error
		ctrlErr := c.Control(func(fd uintptr) {
			sockErr = windows.SetsockoptInt(windows.Handle(fd), windows.IPPROTO_IP, ipUnicastIF, indexNetworkOrder)
		})
		if ctrlErr != nil {
			return ctrlErr
		}
		return sockErr
	}
}
```

- [ ] **Step 2: Write `bind_linux.go`**

Create `internal/netiface/bind_linux.go`:

```go
//go:build linux

package netiface

import (
	"fmt"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// BindControl returns a net.Dialer.Control callback that forces every dial
// through it to leave via the named interface (SO_BINDTODEVICE), the
// interface's existence re-checked fresh on every call - see
// bind_windows.go's doc comment for why this is deliberately never cached.
// No network-byte-order quirk here (unlike Windows' IP_UNICAST_IF):
// SO_BINDTODEVICE takes the interface name directly as a string.
func BindControl(name string) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, c syscall.RawConn) error {
		if _, err := net.InterfaceByName(name); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrInterfaceUnavailable, name, err)
		}

		var sockErr error
		ctrlErr := c.Control(func(fd uintptr) {
			sockErr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, name)
		})
		if ctrlErr != nil {
			return ctrlErr
		}
		return sockErr
	}
}
```

- [ ] **Step 3: Write `bind_darwin.go`**

Create `internal/netiface/bind_darwin.go`:

```go
//go:build darwin

package netiface

import (
	"fmt"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// BindControl returns a net.Dialer.Control callback that forces every dial
// through it to leave via the named interface (IP_BOUND_IF, by index - no
// network-byte-order quirk here, unlike Windows' IP_UNICAST_IF), the
// interface re-checked fresh on every call - see bind_windows.go's doc
// comment for why this is deliberately never cached.
func BindControl(name string) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, c syscall.RawConn) error {
		ifc, err := net.InterfaceByName(name)
		if err != nil {
			return fmt.Errorf("%w: %s: %v", ErrInterfaceUnavailable, name, err)
		}

		var sockErr error
		ctrlErr := c.Control(func(fd uintptr) {
			sockErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_BOUND_IF, ifc.Index)
		})
		if ctrlErr != nil {
			return ctrlErr
		}
		return sockErr
	}
}
```

- [ ] **Step 4: Build for the current OS (Windows)**

Run: `go build ./internal/netiface/...`
Expected: no output, exit 0.

- [ ] **Step 5: Cross-compile-check Linux and macOS**

Run:
```
GOOS=linux GOARCH=amd64 go build ./internal/netiface/...
GOOS=darwin GOARCH=amd64 go build ./internal/netiface/...
```
(On Windows, set via PowerShell's `$env:GOOS='linux'; $env:GOARCH='amd64'; go build ./internal/netiface/...` in one call, then reset `$env:GOOS=''; $env:GOARCH=''` afterward so later steps in this plan build for Windows again - or use the Bash tool, which supports the `VAR=x cmd` inline form directly.)
Expected: both succeed with no output.

- [ ] **Step 6: Commit**

```bash
git add internal/netiface/bind_windows.go internal/netiface/bind_linux.go internal/netiface/bind_darwin.go
git commit -m "netiface: add per-OS BindControl (IP_UNICAST_IF/SO_BINDTODEVICE/IP_BOUND_IF)"
```

---

## Task 3: `internal/checks` — extend `NetConfig`, `classifyNetError`, drop dead `localAddr`

**Files:**
- Modify: `internal/checks/checks.go`
- Test: `internal/checks/checks_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/checks/checks_test.go`:

```go
func TestClassifyNetErrorInterfaceUnavailable(t *testing.T) {
	err := fmt.Errorf("dial tcp: %w: eth-test: interface not found", netiface.ErrInterfaceUnavailable)
	if got := classifyNetError(err); got != "network interface unavailable" {
		t.Errorf("classifyNetError(%v) = %q, want %q", err, got, "network interface unavailable")
	}
}
```

Add `"fmt"` and `"pingachock/internal/netiface"` to the existing import block at the top of `internal/checks/checks_test.go` (currently `"context"`, `"errors"`, `"net"`, `"testing"`, `"time"`).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/checks/... -run TestClassifyNetErrorInterfaceUnavailable -v`
Expected: FAIL - classification comes back `"connection failed"`, not `"network interface unavailable"`.

- [ ] **Step 3: Write the implementation**

In `internal/checks/checks.go`:

Replace the import block:

```go
import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"syscall"
	"time"
)
```

with:

```go
import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"syscall"
	"time"

	"pingachock/internal/netiface"
)
```

Replace the `NetConfig` type and its doc comment:

```go
// NetConfig pins checks to a specific network interface, set by the
// operator via `configure` (see internal/netiface). LocalAddr is nil and
// Resolver is nil when no interface was selected - checkers then fall back
// to whatever the OS/Go default would do, unchanged from before this
// existed.
type NetConfig struct {
	LocalAddr net.IP
	Resolver  *net.Resolver
}
```

with:

```go
// NetConfig pins checks to a specific network interface, set by the
// operator via `configure` (see internal/netiface). Every field is the
// zero value when no interface was selected - checkers then fall back to
// whatever the OS/Go default would do, unchanged from before this design
// existed. See
// docs/superpowers/specs/2026-08-13-vpn-resilient-node-networking-design.md.
type NetConfig struct {
	// LocalAddr is used only as an address-family preference hint when
	// resolveIP picks among multiple DNS answers (see pickPreferredIP) -
	// resolved once at agent startup from the pinned interface's address at
	// that moment. Never used for socket binding directly (see Bind) - this
	// field staying stale for the life of a long-running agent process is
	// harmless, since only its address *family* (IPv4 vs IPv6) is ever
	// consulted, and that's a static property of an interface that doesn't
	// change just because its specific address does.
	LocalAddr net.IP

	Resolver *net.Resolver

	// Bind, when set, forces every dial through it to leave via a specific
	// pinned network interface - not just a specific source address -
	// re-verified fresh on every single call, so neither a changed address
	// (DHCP renewal) nor a removed interface needs anything refreshed here.
	// See internal/netiface's per-OS BindControl. nil means "no interface
	// pinned" - the OS picks the route the same as always.
	Bind BindFunc

	// InterfaceName is the pinned interface's name, for the checkers that
	// can't use Bind directly and need to resolve their own fresh, literal
	// address right before use instead: PingChecker (shells out to the OS
	// ping binary) and VLESSChecker (builds an xray-core config's
	// sendThrough field). See internal/netiface.ByName. Empty means "no
	// interface pinned."
	InterfaceName string
}

// BindFunc is the shape of net.Dialer.Control - named here so
// internal/netiface's per-OS implementations and internal/poller's
// PathSelfTest (see the design doc's Part 4) don't each need their own
// copy of this signature.
type BindFunc = func(network, address string, c syscall.RawConn) error
```

Replace `classifyNetError`'s body - add the interface-unavailable check as the first case after the nil check:

```go
func classifyNetError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, netiface.ErrInterfaceUnavailable) {
		return "network interface unavailable"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "connection refused"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns resolution failed"
	}
	var hostErr x509.HostnameError
	var authErr x509.UnknownAuthorityError
	var certErr x509.CertificateInvalidError
	if errors.As(err, &hostErr) || errors.As(err, &authErr) || errors.As(err, &certErr) {
		return "certificate verification failed"
	}
	return "connection failed"
}
```

Delete the now-dead `localAddr` function entirely (it will have no remaining callers after Task 4 and Task 5 - deleting it now is fine, the package still compiles since Go doesn't error on an unused top-level function, and this keeps this task self-contained instead of leaving known-dead code for a later task to clean up):

```go
// localAddr builds the right net.Addr type for the given network ("tcp...",
// "udp...") - net.Dialer.LocalAddr must match the dial network's address
// family or the dial fails outright.
func localAddr(network string, ip net.IP) net.Addr {
	if ip == nil {
		return nil
	}
	switch {
	case len(network) >= 3 && network[:3] == "tcp":
		return &net.TCPAddr{IP: ip}
	case len(network) >= 3 && network[:3] == "udp":
		return &net.UDPAddr{IP: ip}
	default:
		return &net.IPAddr{IP: ip}
	}
}
```

Also add a new helper right after `resolveIP` (used by Task 5's `PingChecker` and `TLSChecker`'s diagnostic ping, both of which shell out to an external tool and need a literal `netiface.Interface`, not a `Control` callback):

```go
// resolveBoundInterface returns netCfg's pinned interface's current state,
// resolved fresh - never cached - for the checkers that shell out to an
// external tool needing a literal address/interface-name argument
// (PingChecker, and TLSChecker's own diagnostic ping) rather than being
// able to use netCfg.Bind's Control-based binding directly. Returns the
// zero Interface{} with a nil error when no interface is pinned at all -
// callers should treat that exactly like "unbound", not an error.
func resolveBoundInterface(netCfg NetConfig) (netiface.Interface, error) {
	if netCfg.InterfaceName == "" {
		return netiface.Interface{}, nil
	}
	return netiface.ByName(netCfg.InterfaceName)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/checks/... -v`
Expected: this will FAIL to compile at this point - `tcp.go`, `http.go`, `dns.go`, `tls.go`, `upgrade.go` still call the now-deleted `localAddr`. That's expected; Task 4 fixes it. Confirm the *compile error* specifically names `localAddr` as undefined (not some other break) before moving on.

- [ ] **Step 5: Commit**

This task's change is not independently buildable (by design - Task 4 is its direct continuation and the two together are one atomic unit of work). Do not commit yet; proceed straight to Task 4, then commit both together at the end of Task 4's Step 3.

---

## Task 4: swap `LocalAddr` for `Control` in tcp/http/dns/tls/upgrade

**Files:**
- Modify: `internal/checks/tcp.go`
- Modify: `internal/checks/http.go`
- Modify: `internal/checks/dns.go`
- Modify: `internal/checks/tls.go`
- Modify: `internal/checks/upgrade.go`

This task has no new test of its own - it's a mechanical, behavior-preserving swap (the zero-value case, `netCfg == NetConfig{}`, is identical before and after: nil `Control` and nil `LocalAddr` both mean "OS default", exactly the case every existing test in this package already exercises). The check that matters is that the full existing suite still passes unchanged, in Step 4.

- [ ] **Step 1: `tcp.go`**

In `internal/checks/tcp.go`, replace:

```go
	probeTarget, reportedIP := resolveIP(ctx, netCfg.Resolver, target, time.Duration(p.TimeoutMs)*time.Millisecond, netCfg.LocalAddr)
	addr := net.JoinHostPort(probeTarget, strconv.Itoa(p.Port))
	dialer := net.Dialer{
		Timeout:   time.Duration(p.TimeoutMs) * time.Millisecond,
		Resolver:  netCfg.Resolver,
		LocalAddr: localAddr("tcp", netCfg.LocalAddr),
	}
```

with:

```go
	probeTarget, reportedIP := resolveIP(ctx, netCfg.Resolver, target, time.Duration(p.TimeoutMs)*time.Millisecond, netCfg.LocalAddr)
	addr := net.JoinHostPort(probeTarget, strconv.Itoa(p.Port))
	dialer := net.Dialer{
		Timeout:  time.Duration(p.TimeoutMs) * time.Millisecond,
		Resolver: netCfg.Resolver,
		Control:  netCfg.Bind,
	}
```

- [ ] **Step 2: `http.go`**

In `internal/checks/http.go`, replace:

```go
	dialer := &net.Dialer{Resolver: netCfg.Resolver, LocalAddr: localAddr("tcp", netCfg.LocalAddr)}
```

with:

```go
	dialer := &net.Dialer{Resolver: netCfg.Resolver, Control: netCfg.Bind}
```

- [ ] **Step 3: `dns.go`**

In `internal/checks/dns.go`, replace:

```go
	if p.Resolver != "" {
		resolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				d := net.Dialer{Timeout: time.Duration(p.TimeoutMs) * time.Millisecond, LocalAddr: localAddr(network, netCfg.LocalAddr)}
				return d.DialContext(ctx, network, net.JoinHostPort(p.Resolver, "53"))
			},
		}
	}
```

with:

```go
	if p.Resolver != "" {
		resolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				d := net.Dialer{Timeout: time.Duration(p.TimeoutMs) * time.Millisecond, Control: netCfg.Bind}
				return d.DialContext(ctx, network, net.JoinHostPort(p.Resolver, "53"))
			},
		}
	}
```

- [ ] **Step 4: `tls.go`**

In `internal/checks/tls.go`, replace:

```go
	dialer := net.Dialer{
		Timeout:   timeout,
		LocalAddr: localAddr("tcp", netCfg.LocalAddr),
	}
```

with:

```go
	dialer := net.Dialer{
		Timeout: timeout,
		Control: netCfg.Bind,
	}
```

- [ ] **Step 5: `upgrade.go`**

In `internal/checks/upgrade.go`, replace:

```go
	dialer := net.Dialer{Timeout: timeout, LocalAddr: localAddr("tcp", netCfg.LocalAddr)}
```

with:

```go
	dialer := net.Dialer{Timeout: timeout, Control: netCfg.Bind}
```

- [ ] **Step 6: Build**

Run: `go build ./...`
Expected: this will still FAIL - `ping.go`'s and `tls.go`'s `diagnosticPingReceived` still call `pingArgs(..., netCfg.LocalAddr)` with the old `net.IP`-based signature, and Task 5 changes that signature. Confirm the error is specifically about `pingArgs`'s argument type before moving on.

- [ ] **Step 7: Commit (together with Task 3)**

```bash
git add internal/checks/checks.go internal/checks/checks_test.go internal/checks/tcp.go internal/checks/http.go internal/checks/dns.go internal/checks/tls.go internal/checks/upgrade.go
git commit -m "checks: bind tcp/http/dns/tls/upgrade to the pinned interface itself, not a cached address"
```

(This intentionally bundles Task 3 and Task 4 into one commit - Task 3 alone doesn't compile, so it isn't a meaningful standalone commit boundary.)

---

## Task 5: `ping.go` and `tls.go`'s diagnostic ping — resolve the interface fresh

**Files:**
- Modify: `internal/checks/ping.go`
- Modify: `internal/checks/tls.go`
- Test: `internal/checks/ping_test.go`

`pingArgs` changes its last parameter from a `net.IP` to a `netiface.Interface`, so it can pass an interface *name* to Linux's `ping -I` (which accepts either an address or, critically, an interface name - and when given a name, `ping -I <name>` invokes `SO_BINDTODEVICE` internally, the same strong per-interface guarantee `BindControl` uses, for free). Windows and macOS `ping` have no interface-name flag, so those two continue passing a resolved address via `-S`, same as before - the only actual change for them is that the address is resolved *fresh* right before each check, not read from a stale field cached at agent startup.

- [ ] **Step 1: Write the failing test**

Replace `TestParsePingOutputWindowsEnglish` through the end of the existing `pingArgs`-adjacent tests is not needed - `pingArgs` had no direct test before this change. Add a new test to `internal/checks/ping_test.go` (append at the end of the file):

```go
func TestPingArgsLinuxUsesInterfaceNameNotAddress(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("pingArgs' Linux branch only runs its interface-name behavior on GOOS=linux")
	}
	ifc := netiface.Interface{Name: "eth-test", Addrs: []net.IP{net.ParseIP("192.168.1.50")}}
	args := pingArgs("203.0.113.5", 4, 5000, ifc)

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-I eth-test") {
		t.Errorf("pingArgs args = %v, want \"-I eth-test\" (bind by interface name, not address)", args)
	}
	if strings.Contains(joined, "192.168.1.50") {
		t.Errorf("pingArgs args = %v, want the interface's address NOT to appear - -I takes the name directly on Linux", args)
	}
}

func TestPingArgsNoInterfacePinnedOmitsBindFlag(t *testing.T) {
	args := pingArgs("203.0.113.5", 4, 5000, netiface.Interface{})
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "-S") || strings.Contains(joined, "-I") {
		t.Errorf("pingArgs args = %v, want no -S/-I flag when no interface is pinned", args)
	}
}
```

`internal/checks/ping_test.go`'s current import block is just:

```go
import (
	"context"
	"testing"
)
```

Replace it with:

```go
import (
	"context"
	"net"
	"pingachock/internal/netiface"
	"strings"
	"testing"
)
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/checks/... -run 'TestPingArgs' -v`
Expected: FAIL to compile - `pingArgs`'s current signature takes `net.IP`, not `netiface.Interface`.

- [ ] **Step 3: Write the implementation**

In `internal/checks/ping.go`, replace:

```go
func pingArgs(target string, count, timeoutMs int, localAddr net.IP) []string {
	if runtime.GOOS == "windows" {
		args := []string{"ping", "-n", strconv.Itoa(count), "-w", strconv.Itoa(timeoutMs)}
		if localAddr != nil {
			args = append(args, "-S", localAddr.String())
		}
		return append(args, target)
	}

	timeoutSec := timeoutMs / 1000
	if timeoutSec < 1 {
		timeoutSec = 1
	}
	args := []string{"ping", "-c", strconv.Itoa(count), "-W", strconv.Itoa(timeoutSec)}
	if localAddr != nil {
		if runtime.GOOS == "darwin" {
			args = append(args, "-S", localAddr.String()) // BSD ping: source address, not interface name
		} else {
			args = append(args, "-I", localAddr.String()) // iputils ping accepts an address here too
		}
	}
	return append(args, target)
}
```

with:

```go
// pingArgs' last parameter used to be a bare net.IP resolved once at agent
// startup and cached in NetConfig.LocalAddr - now it's the pinned
// interface's current state, resolved fresh by the caller on every single
// Run() (see resolveBoundInterface in checks.go), so a DHCP-renewed address
// is never stale here. iface's zero value (Interface{}) means "no interface
// pinned" - identical to the old localAddr == nil case.
func pingArgs(target string, count, timeoutMs int, iface netiface.Interface) []string {
	if runtime.GOOS == "windows" {
		args := []string{"ping", "-n", strconv.Itoa(count), "-w", strconv.Itoa(timeoutMs)}
		if addr := iface.PreferredAddr(); addr != nil {
			args = append(args, "-S", addr.String())
		}
		return append(args, target)
	}

	timeoutSec := timeoutMs / 1000
	if timeoutSec < 1 {
		timeoutSec = 1
	}
	args := []string{"ping", "-c", strconv.Itoa(count), "-W", strconv.Itoa(timeoutSec)}
	if iface.Name != "" {
		if runtime.GOOS == "darwin" {
			// BSD ping has no interface-name flag - a resolved address is
			// the best available, same as Windows above.
			if addr := iface.PreferredAddr(); addr != nil {
				args = append(args, "-S", addr.String())
			}
		} else {
			// GNU/iputils ping's -I accepts an interface *name* directly,
			// which invokes SO_BINDTODEVICE internally - the same strong,
			// routing-table-overriding guarantee BindControl uses
			// elsewhere, for free, and stronger than binding a source
			// address alone would be.
			args = append(args, "-I", iface.Name)
		}
	}
	return append(args, target)
}
```

Update the import block at the top of `internal/checks/ping.go` - add `"pingachock/internal/netiface"`:

```go
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os/exec"
	"pingachock/internal/netiface"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)
```

(Go's `gofmt`/`goimports` groups this into its own block after the stdlib imports on the next `go build`/`gofmt -w`; leaving it inline in the single block above is also valid Go and compiles the same either way - either is fine here.)

Now update `PingChecker.Run` itself. Replace:

```go
	resolvedTarget, reportedIP := resolveIP(ctx, netCfg.Resolver, target, time.Duration(p.TimeoutMs)*time.Millisecond, netCfg.LocalAddr)
	resolutionFailed := net.ParseIP(target) == nil && reportedIP == ""

	overall := time.Duration(p.TimeoutMs)*time.Millisecond*time.Duration(p.Count) + 5*time.Second
	cmdCtx, cancel := context.WithTimeout(ctx, overall)
	defer cancel()

	args := pingArgs(resolvedTarget, p.Count, p.TimeoutMs, netCfg.LocalAddr)
```

with:

```go
	boundIface, err := resolveBoundInterface(netCfg)
	if err != nil {
		msg := classifyNetError(err)
		return Result{Success: false, ErrorMessage: &msg}
	}

	resolvedTarget, reportedIP := resolveIP(ctx, netCfg.Resolver, target, time.Duration(p.TimeoutMs)*time.Millisecond, boundIface.PreferredAddr())
	resolutionFailed := net.ParseIP(target) == nil && reportedIP == ""

	overall := time.Duration(p.TimeoutMs)*time.Millisecond*time.Duration(p.Count) + 5*time.Second
	cmdCtx, cancel := context.WithTimeout(ctx, overall)
	defer cancel()

	args := pingArgs(resolvedTarget, p.Count, p.TimeoutMs, boundIface)
```

Note the `err` here shadows nothing - `PingChecker.Run` didn't previously declare an `err` this early; check that no later `err :=` in the same function now needs to become plain `=` because of this new declaration (it doesn't - the next `err` usage in this file, if any, is inside `parsePingOutput`'s own scope, a different function entirely).

Now `internal/checks/tls.go`'s `diagnosticPingReceived` needs the same treatment. Replace:

```go
func diagnosticPingReceived(ctx context.Context, netCfg NetConfig, target string) int {
	const pingCount = 1
	const pingTimeoutMs = 1500
	overall := time.Duration(pingTimeoutMs)*time.Millisecond*time.Duration(pingCount) + 3*time.Second
	cmdCtx, cancel := context.WithTimeout(ctx, overall)
	defer cancel()

	args := pingArgs(target, pingCount, pingTimeoutMs, netCfg.LocalAddr)
	cmd := exec.CommandContext(cmdCtx, args[0], args[1:]...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	_ = cmd.Run()

	_, recv, _ := parsePingOutput(out.String())
	return recv
}
```

with:

```go
func diagnosticPingReceived(ctx context.Context, netCfg NetConfig, target string) int {
	boundIface, err := resolveBoundInterface(netCfg)
	if err != nil {
		// The pinned interface is gone - there's no meaningful diagnostic
		// ping to run either way, and the TLS attempt this is diagnosing
		// already failed for the same underlying reason. classifyUnreachable
		// with recv==0 ("ip unreachable") is the closer-to-honest of the two
		// possible outcomes here, not a real detection either way.
		return 0
	}

	const pingCount = 1
	const pingTimeoutMs = 1500
	overall := time.Duration(pingTimeoutMs)*time.Millisecond*time.Duration(pingCount) + 3*time.Second
	cmdCtx, cancel := context.WithTimeout(ctx, overall)
	defer cancel()

	args := pingArgs(target, pingCount, pingTimeoutMs, boundIface)
	cmd := exec.CommandContext(cmdCtx, args[0], args[1:]...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	_ = cmd.Run()

	_, recv, _ := parsePingOutput(out.String())
	return recv
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/checks/... -v`
Expected: PASS - the whole `internal/checks` package, including every pre-existing test (`TestPingArgs*` new ones, plus everything from `ping_test.go`, `tls_test.go`, `upgrade_test.go`, `checks_test.go`; `vless_test.go` still fails at this point - that's Task 6).

- [ ] **Step 5: Commit**

```bash
git add internal/checks/ping.go internal/checks/tls.go internal/checks/ping_test.go
git commit -m "checks: ping.go and tls.go's diagnostic ping resolve the pinned interface fresh, not cached"
```

---

## Task 6: `vless.go` — `sendThrough` on every real outbound

**Files:**
- Modify: `internal/checks/vless.go`
- Modify: `internal/checks/vless_test.go`

Part 2 of the design doc: xray-core has no interface-*identity* bind option in its config schema, only a literal source IP via each outbound's `sendThrough` field - the closest equivalent to `BindControl`, applied by `patchInbound` the same way it already force-overrides `inbounds` and `log`.

- [ ] **Step 1: Write the failing test**

Update the four existing `patchInbound(...)` calls in `internal/checks/vless_test.go` to pass an empty `sendThrough` (preserving their current behavior exactly - none of them are testing the new feature):

Replace `patched, err := patchInbound(input, 12345)` (appears at both line 12 and line 80) with `patched, err := patchInbound(input, 12345, "")` - **both occurrences**, in `TestPatchInboundAddsSocksWhenNoneExists` and `TestPatchInboundOverridesLogConfig`.

Replace `patched, err := patchInbound(input, 55555)` (line 46, in `TestPatchInboundReplacesExistingInbounds`) with `patched, err := patchInbound(input, 55555, "")`.

Replace `_, err := patchInbound(json.RawMessage(`+"`"+`not json`+"`"+`), 1234)` (line 103, in `TestPatchInboundRejectsInvalidJSON`) with `_, err := patchInbound(json.RawMessage(`+"`"+`not json`+"`"+`), 1234, "")`.

Then append this new test to the end of `internal/checks/vless_test.go`:

```go
// TestPatchInboundSetsSendThroughOnRealOutboundsOnly: xray-core has no
// interface-identity bind option in its config schema, only a literal
// source IP via each outbound's sendThrough field - the VLESS equivalent
// of BindControl's interface pinning. "blackhole" never opens a real
// connection, so it must not get one.
func TestPatchInboundSetsSendThroughOnRealOutboundsOnly(t *testing.T) {
	input := json.RawMessage(`{
		"outbounds": [
			{"protocol": "vless", "settings": {}},
			{"protocol": "freedom", "tag": "direct"},
			{"protocol": "blackhole", "tag": "block"}
		]
	}`)
	patched, err := patchInbound(input, 12345, "192.168.1.50")
	if err != nil {
		t.Fatalf("patchInbound() error = %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(patched, &doc); err != nil {
		t.Fatalf("unmarshal patched config: %v", err)
	}
	outbounds, ok := doc["outbounds"].([]any)
	if !ok || len(outbounds) != 3 {
		t.Fatalf("outbounds = %v, want exactly 3 entries preserved", doc["outbounds"])
	}

	for _, raw := range outbounds {
		entry := raw.(map[string]any)
		protocol := entry["protocol"].(string)
		sendThrough, has := entry["sendThrough"]
		if protocol == "blackhole" {
			if has {
				t.Errorf("blackhole outbound got sendThrough = %v, want none - it never opens a real connection", sendThrough)
			}
			continue
		}
		if sendThrough != "192.168.1.50" {
			t.Errorf("%s outbound sendThrough = %v, want %q", protocol, sendThrough, "192.168.1.50")
		}
	}
}

// TestPatchInboundEmptySendThroughLeavesOutboundsUntouched: an empty
// sendThrough (no interface pinned - see VLESSChecker.Run) must not add the
// field at all, not add it as an empty string - an empty sendThrough in a
// real xray-core config is itself a (harmless but pointless) config
// oddity, and leaving outbounds completely untouched when there's nothing
// meaningful to bind to is the more honest behavior.
func TestPatchInboundEmptySendThroughLeavesOutboundsUntouched(t *testing.T) {
	input := json.RawMessage(`{"outbounds": [{"protocol": "vless"}]}`)
	patched, err := patchInbound(input, 12345, "")
	if err != nil {
		t.Fatalf("patchInbound() error = %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(patched, &doc); err != nil {
		t.Fatalf("unmarshal patched config: %v", err)
	}
	outbounds := doc["outbounds"].([]any)
	entry := outbounds[0].(map[string]any)
	if _, has := entry["sendThrough"]; has {
		t.Errorf("outbound got sendThrough = %v with an empty pin, want the field entirely absent", entry["sendThrough"])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/checks/... -run TestPatchInbound -v`
Expected: FAIL to compile - `patchInbound` still takes 2 arguments.

- [ ] **Step 3: Write the implementation**

In `internal/checks/vless.go`, replace `patchInbound`'s signature and body:

```go
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
```

with:

```go
func patchInbound(config json.RawMessage, port int, sendThrough string) (json.RawMessage, error) {
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

	if sendThrough != "" {
		patched, err := patchOutboundsSendThrough(doc["outbounds"], sendThrough)
		if err != nil {
			return nil, fmt.Errorf("outbounds: %w", err)
		}
		doc["outbounds"] = patched
	}

	return json.Marshal(doc)
}

// patchOutboundsSendThrough forces every outbound that actually performs
// network egress to bind through sendThrough - the pinned physical
// interface's current address, resolved fresh right before every check
// run (see VLESSChecker.Run), never a cached value, for the same
// staleness reason internal/netiface.BindControl re-verifies its
// interface on every single call. xray-core has no interface-*identity*
// bind option in its config schema (only a literal source IP via
// sendThrough), unlike every other checker's Control-based approach, so
// this is the closest available equivalent. "blackhole" is explicitly
// skipped - it never opens a real connection, so it has nothing to bind.
func patchOutboundsSendThrough(outbounds json.RawMessage, sendThrough string) (json.RawMessage, error) {
	if len(outbounds) == 0 {
		return outbounds, nil
	}
	var entries []map[string]any
	if err := json.Unmarshal(outbounds, &entries); err != nil {
		return nil, fmt.Errorf("not a valid outbounds array: %w", err)
	}
	for _, entry := range entries {
		if protocol, _ := entry["protocol"].(string); protocol == "blackhole" {
			continue
		}
		entry["sendThrough"] = sendThrough
	}
	return json.Marshal(entries)
}
```

Now update `VLESSChecker.Run` to resolve the pinned interface's current address and pass it through. Replace:

```go
	patchedConfig, err := patchInbound(p.Config, port)
	if err != nil {
		msg := "invalid config: " + err.Error()
		return Result{Success: false, ErrorMessage: &msg}
	}
```

with:

```go
	var sendThrough string
	if netCfg.InterfaceName != "" {
		ifc, err := netiface.ByName(netCfg.InterfaceName)
		if err != nil {
			msg := classifyNetError(err)
			return Result{Success: false, ErrorMessage: &msg}
		}
		if addr := ifc.PreferredAddr(); addr != nil {
			sendThrough = addr.String()
		}
	}

	patchedConfig, err := patchInbound(p.Config, port, sendThrough)
	if err != nil {
		msg := "invalid config: " + err.Error()
		return Result{Success: false, ErrorMessage: &msg}
	}
```

Add `"pingachock/internal/netiface"` to `internal/checks/vless.go`'s import block:

```go
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
	"pingachock/internal/netiface"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/checks/... -v`
Expected: PASS - the entire `internal/checks` package now.

- [ ] **Step 5: Commit**

```bash
git add internal/checks/vless.go internal/checks/vless_test.go
git commit -m "checks: VLESSChecker forces sendThrough to the pinned interface on every real outbound"
```

---

## Task 7: `cmd/agent/main.go` — rewrite `buildNetConfig`

**Files:**
- Modify: `cmd/agent/main.go`

No new automated test for this one - `buildNetConfig` reads real OS interface state and `config.Config`, both awkward to fake meaningfully at this layer, and its correctness is already covered end-to-end by every `internal/checks` test plus the manual verification in Task 12. Keep the change small and match the design doc's Part 1 exactly.

- [ ] **Step 1: Write the implementation**

In `cmd/agent/main.go`, replace `buildNetConfig`:

```go
// buildNetConfig turns the interface/DNS settings picked at setup time into
// the resolver+dialer checks actually use. See internal/checks.NetConfig.
func buildNetConfig(cfg config.Config) checks.NetConfig {
	var netCfg checks.NetConfig
	if cfg.LocalAddr != "" {
		netCfg.LocalAddr = net.ParseIP(cfg.LocalAddr)
	}
	if len(cfg.DNSServers) > 0 {
		servers := cfg.DNSServers
		localIP := netCfg.LocalAddr
		netCfg.Resolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				d := net.Dialer{Timeout: 5 * time.Second}
				if localIP != nil {
					if strings.HasPrefix(network, "tcp") {
						d.LocalAddr = &net.TCPAddr{IP: localIP}
					} else {
						d.LocalAddr = &net.UDPAddr{IP: localIP}
					}
				}
				// First configured server; if it's unreachable the lookup
				// just fails for that check the way any DNS outage would.
				return d.DialContext(ctx, network, net.JoinHostPort(servers[0], "53"))
			},
		}
	}
	return netCfg
}
```

with:

```go
// buildNetConfig turns the interface/DNS settings picked at setup time into
// the resolver+dialer checks actually use. See internal/checks.NetConfig
// and docs/superpowers/specs/2026-08-13-vpn-resilient-node-networking-design.md.
func buildNetConfig(cfg config.Config) checks.NetConfig {
	var netCfg checks.NetConfig
	if cfg.InterfaceName != "" {
		netCfg.InterfaceName = cfg.InterfaceName
		netCfg.Bind = netiface.BindControl(cfg.InterfaceName)
		if ifc, err := netiface.ByName(cfg.InterfaceName); err == nil {
			netCfg.LocalAddr = ifc.PreferredAddr()
		}
		// If ByName fails right now (interface briefly gone at agent
		// startup, e.g. a Wi-Fi adapter still coming up), LocalAddr just
		// stays nil - it only ever affects which DNS answer's family gets
		// picked, a much smaller blast radius than a failed dial. Bind and
		// InterfaceName above (what actually matters) are unaffected: Bind
		// re-checks the interface itself on every real dial, and will start
		// working the moment the interface is actually up, no restart
		// needed.
	} else if cfg.LocalAddr != "" {
		// Back-compat: a config saved by an agent build from before
		// InterfaceName was read here. Degrades to the old, address-only
		// behavior (no Control-based binding) rather than losing interface
		// pinning entirely - operators on an old config still get *some*
		// protection until they next run `configure`.
		netCfg.LocalAddr = net.ParseIP(cfg.LocalAddr)
	}
	if len(cfg.DNSServers) > 0 {
		servers := cfg.DNSServers
		bind := netCfg.Bind
		netCfg.Resolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				d := net.Dialer{Timeout: 5 * time.Second, Control: bind}
				// First configured server; if it's unreachable the lookup
				// just fails for that check the way any DNS outage would.
				return d.DialContext(ctx, network, net.JoinHostPort(servers[0], "53"))
			},
		}
	}
	return netCfg
}
```

Check `cmd/agent/main.go`'s import block for `"strings"` - it was only used in the old `buildNetConfig` for the `strings.HasPrefix(network, "tcp")` check being removed here. Search the rest of the file (`grep -n "strings\." cmd/agent/main.go`) for any other usage before removing the import - if there are other call sites elsewhere in this large file (likely, given its size), leave the import alone; only remove it if `buildNetConfig` was the only user.

- [ ] **Step 2: Build**

Run: `go build ./...`
Expected: no output, exit 0. If `"strings"` turned out to be unused after all, the build fails with `"strings" imported and not used` - remove that one import line and rebuild.

- [ ] **Step 3: Manual smoke check**

Run the agent's `configure` command against this machine's real network interfaces and confirm it still completes normally (this exercises `chooseInterface`/`probeInterface`, unchanged by this task, plus the now-rewritten `buildNetConfig` indirectly via a subsequent `run`):

```
go run ./cmd/agent configure
```

Pick any physical interface when prompted. Expected: completes without error, same prompts as before this task.

- [ ] **Step 4: Commit**

```bash
git add cmd/agent/main.go
git commit -m "agent: buildNetConfig binds by interface identity, not a startup-time address snapshot"
```

---

## Task 8: `internal/poller` — `PathSelfTest`

**Files:**
- Create: `internal/poller/selftest.go`
- Create: `internal/poller/selftest_test.go`

Part 4 of the design doc. `classifyPathSuspect` is pulled out as a pure function specifically so this task's test needs no real dialing (mirrors `internal/checks/tls.go`'s `classifyUnreachable`).

- [ ] **Step 1: Write the failing test**

Create `internal/poller/selftest_test.go`:

```go
package poller

import "testing"

func TestClassifyPathSuspect(t *testing.T) {
	cases := []struct {
		name               string
		boundOK, unboundOK bool
		want               bool
	}{
		{"no VPN, both paths up - not suspect", true, true, false},
		{"no VPN, both paths down (real outage/censorship, not our problem) - not suspect", false, false, false},
		{"bound path somehow still works when unbound doesn't - unusual, still not suspect", true, false, false},
		{"bound path fails while unbound succeeds - the actual interception signature", false, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyPathSuspect(tc.boundOK, tc.unboundOK); got != tc.want {
				t.Errorf("classifyPathSuspect(%v, %v) = %v, want %v", tc.boundOK, tc.unboundOK, got, tc.want)
			}
		})
	}
}

func TestPathSelfTestSuspectDefaultsFalse(t *testing.T) {
	pt := &PathSelfTest{}
	if pt.Suspect() {
		t.Error("Suspect() = true on a freshly constructed PathSelfTest that has never ticked, want false")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/poller/... -run 'TestClassifyPathSuspect|TestPathSelfTest' -v`
Expected: FAIL to compile - `classifyPathSuspect` and `PathSelfTest` don't exist yet.

- [ ] **Step 3: Write the implementation**

Create `internal/poller/selftest.go`:

```go
package poller

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"time"

	"pingachock/internal/checks"
)

// PathSelfTest is the background safety net for the one class of VPN/proxy
// interference explicit interface binding (checks.NetConfig.Bind) can't
// defeat: system-wide packet-filter-level capture (a Windows WFP-based
// killswitch, for example) that intercepts traffic regardless of what
// interface a socket explicitly asked to bind through. See
// docs/superpowers/specs/2026-08-13-vpn-resilient-node-networking-design.md
// Part 4.
//
// This is deliberately a *differential* test - bound vs unbound against the
// exact same targets at the exact same moment - not "is this target
// reachable" against some assumed-always-up baseline. With no interference
// at all, a bound dial and an unbound dial take the same physical path and
// always agree, so this can never false-positive just because a target is
// itself down or genuinely censored that day - it only fires when the two
// diverge, which only happens when something treats the bound path
// differently from the unbound one.
type PathSelfTest struct {
	// Bind, when nil, disables the self-test entirely - matches every
	// other piece of this design's "no interface pinned -> no behavior
	// change" rule.
	Bind     checks.BindFunc
	Interval time.Duration
	Targets  []string // "host:port", e.g. "1.1.1.1:443"
	Log      *slog.Logger

	mu      sync.RWMutex
	suspect bool
}

// Suspect reports whether the most recent tick found the bound path
// failing while the unbound path succeeded - Poller withholds result
// submission while this is true (see poller.go).
func (t *PathSelfTest) Suspect() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.suspect
}

// Run blocks, self-testing on its own ticker until ctx is cancelled -
// entirely independent of, and never blocking, the main poll/execute loop
// (Poller.Run), which only ever reads Suspect().
func (t *PathSelfTest) Run(ctx context.Context) {
	if t.Bind == nil || len(t.Targets) == 0 {
		return
	}
	interval := t.Interval
	if interval <= 0 {
		interval = 2 * time.Minute
	}

	t.tick(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.tick(ctx)
		}
	}
}

func (t *PathSelfTest) tick(ctx context.Context) {
	suspect := false
	for _, target := range t.Targets {
		boundOK := dialOK(ctx, target, t.Bind)
		unboundOK := dialOK(ctx, target, nil)
		if classifyPathSuspect(boundOK, unboundOK) {
			suspect = true
			break
		}
	}

	t.mu.Lock()
	wasSuspect := t.suspect
	t.suspect = suspect
	t.mu.Unlock()

	if suspect == wasSuspect {
		return
	}
	if t.Log == nil {
		return
	}
	if suspect {
		t.Log.Warn("path self-test: bound path failing while unbound path succeeds - suspected VPN/proxy interception below the socket layer, withholding check results until this clears")
	} else {
		t.Log.Info("path self-test: bound path OK again, resuming normal result submission")
	}
}

// classifyPathSuspect is tick's decision logic, split out so it's
// unit-testable without any real dialing - mirrors how
// internal/checks/tls.go separates classifyUnreachable from its own
// dialing plumbing.
func classifyPathSuspect(boundOK, unboundOK bool) bool {
	return !boundOK && unboundOK
}

func dialOK(ctx context.Context, target string, bind checks.BindFunc) bool {
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	d := net.Dialer{Control: bind}
	conn, err := d.DialContext(dialCtx, "tcp", target)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/poller/... -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/poller/selftest.go internal/poller/selftest_test.go
git commit -m "poller: add PathSelfTest, the background bound-vs-unbound interception safety net"
```

---

## Task 9: `internal/poller/poller.go` — withhold results while path is suspect

**Files:**
- Modify: `internal/poller/poller.go`
- Create: `internal/poller/poller_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/poller/poller_test.go`:

```go
package poller

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"pingachock/internal/transport"
)

// fakeTransport is a minimal transport.Transport test double. Poll returns
// its canned jobs exactly once (so a test calling p.tick directly, not
// p.Run, never has to worry about a background ticker); PostResults
// records how many times and with what it was called, for assertions.
type fakeTransport struct {
	jobs        []transport.Job
	polled      bool
	postedCalls int
	lastPosted  []transport.ResultSubmission
}

func (f *fakeTransport) Poll(ctx context.Context, agentVersion string) ([]transport.Job, error) {
	if f.polled {
		return nil, nil
	}
	f.polled = true
	return f.jobs, nil
}

func (f *fakeTransport) PostResults(ctx context.Context, results []transport.ResultSubmission) error {
	f.postedCalls++
	f.lastPosted = results
	return nil
}

// testJob is a "tcp" check against a port nothing listens on (1 - the
// reserved tcpmux port), so it fails fast and deterministically without
// any real network dependency - same trick internal/checks/tls_test.go's
// TestTLSCheckerConnectionRefusedFailsAllAttempts already uses.
func testJob() transport.Job {
	return transport.Job{
		CheckRunID: uuid.New(),
		Type:       "tcp",
		Target:     "127.0.0.1",
		Params:     json.RawMessage(`{"port":1,"timeout_ms":500}`),
	}
}

func TestTickPostsResultsNormallyWhenPathNotSuspect(t *testing.T) {
	ft := &fakeTransport{jobs: []transport.Job{testJob()}}
	p := &Poller{Transport: ft, Log: slog.Default()}

	p.tick(context.Background())

	if ft.postedCalls != 1 {
		t.Fatalf("PostResults called %d times, want 1", ft.postedCalls)
	}
	if len(ft.lastPosted) != 1 {
		t.Fatalf("posted %d results, want 1", len(ft.lastPosted))
	}
}

func TestTickWithholdsResultsWhenPathSuspect(t *testing.T) {
	ft := &fakeTransport{jobs: []transport.Job{testJob()}}
	pathTest := &PathSelfTest{}
	pathTest.suspect = true // simulate an already-detected interference finding, without waiting on a real self-test tick
	p := &Poller{Transport: ft, PathTest: pathTest, Log: slog.Default()}

	p.tick(context.Background())

	if ft.postedCalls != 0 {
		t.Fatalf("PostResults called %d times, want 0 - path self-test currently suspects interference", ft.postedCalls)
	}
}

func TestTickWithNoPathTestConfiguredBehavesAsBefore(t *testing.T) {
	ft := &fakeTransport{jobs: []transport.Job{testJob()}}
	p := &Poller{Transport: ft, Log: slog.Default()} // PathTest left nil, same as every existing deployment before this feature

	p.tick(context.Background())

	if ft.postedCalls != 1 {
		t.Fatalf("PostResults called %d times, want 1 - a nil PathTest must never withhold anything", ft.postedCalls)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/poller/... -run TestTick -v`
Expected: FAIL - `TestTickWithholdsResultsWhenPathSuspect` fails because `Poller` has no `PathTest` field yet (compile error).

- [ ] **Step 3: Write the implementation**

In `internal/poller/poller.go`, add a `PathTest` field to the `Poller` struct. Replace:

```go
type Poller struct {
	Transport     transport.Transport
	Interval      time.Duration
	AgentVersion  string
	MaxConcurrent int
	NetConfig     checks.NetConfig
	Log           *slog.Logger
```

with:

```go
type Poller struct {
	Transport     transport.Transport
	Interval      time.Duration
	AgentVersion  string
	MaxConcurrent int
	NetConfig     checks.NetConfig

	// PathTest, when set, is consulted right before every result
	// submission - while it reports Suspect(), this tick's results are
	// withheld instead of posted (see tick, below). nil (the default,
	// unless cmd/agent wires one up) means "never withhold anything",
	// identical to this feature not existing at all.
	PathTest *PathSelfTest

	Log *slog.Logger
```

Now find `tick`'s submission point. Replace:

```go
	wg.Wait()

	if err := p.Transport.PostResults(ctx, results); err != nil {
```

with:

```go
	wg.Wait()

	if p.PathTest != nil && p.PathTest.Suspect() {
		p.Log.Warn("withholding this tick's results - path self-test currently suspects VPN/proxy interception", "count", len(results))
		return
	}

	if err := p.Transport.PostResults(ctx, results); err != nil {
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/poller/... -v`
Expected: PASS - all of `poller_test.go` and `selftest_test.go`.

- [ ] **Step 5: Commit**

```bash
git add internal/poller/poller.go internal/poller/poller_test.go
git commit -m "poller: withhold a tick's results instead of posting them while PathTest suspects interference"
```

---

## Task 10: `cmd/agent/main.go` — construct and start `PathSelfTest`

**Files:**
- Modify: `cmd/agent/main.go`

- [ ] **Step 1: Write the implementation**

In `cmd/agent/main.go`, find where `pl := &poller.Poller{...}` is constructed (right after the `direct`/`fronted`/`tr` transport setup). Replace:

```go
	pl := &poller.Poller{
		Transport:     tr,
		Interval:      time.Duration(cfg.PollIntervalSeconds) * time.Second,
		AgentVersion:  agentVersion,
		MaxConcurrent: cfg.MaxConcurrentChecks,
		NetConfig:     netCfg,
		Log:           p.log,
		StatePath:     agentstate.Path(baseDir),
	}
```

with:

```go
	pathTest := &poller.PathSelfTest{
		Bind:    netCfg.Bind,
		Targets: []string{"1.1.1.1:443", "8.8.8.8:443"},
		Log:     p.log,
	}
	go pathTest.Run(ctx)

	pl := &poller.Poller{
		Transport:     tr,
		Interval:      time.Duration(cfg.PollIntervalSeconds) * time.Second,
		AgentVersion:  agentVersion,
		MaxConcurrent: cfg.MaxConcurrentChecks,
		NetConfig:     netCfg,
		PathTest:      pathTest,
		Log:           p.log,
		StatePath:     agentstate.Path(baseDir),
	}
```

(`pathTest.Run` no-ops immediately and returns if `netCfg.Bind` is nil - see Task 8's `Run` - so this is safe to always start unconditionally, including on a node with no interface pinned at all; it just never does anything in that case.)

- [ ] **Step 2: Build**

Run: `go build ./...`
Expected: no output, exit 0.

- [ ] **Step 3: Commit**

```bash
git add cmd/agent/main.go
git commit -m "agent: start the background path self-test alongside the main poll loop"
```

---

## Task 11: bot — translate the new `"network interface unavailable"` token

**Files:**
- Modify: `bot/src/pingachock-client.ts`
- Modify: `bot/src/icmp-summary.test.ts`

- [ ] **Step 1: Write the failing test**

Append to `bot/src/icmp-summary.test.ts`:

```typescript
// A pinned interface disappearing (Task 1-2's ErrInterfaceUnavailable,
// surfaced via internal/checks.classifyNetError) is an agent/host problem,
// not a target-reachability one - distinct wording so an operator doesn't
// mistake it for every check target suddenly going down at once.
test('translateCheckError maps the pinned-interface-gone token to Russian', () => {
  assert.equal(
    translateCheckError('network interface unavailable'),
    'сетевой интерфейс узла недоступен (нужно заново запустить configure)'
  );
});
```

- [ ] **Step 2: Run test to verify it fails**

Run (from the `bot/` directory): `npx tsx --test src/icmp-summary.test.ts`
Expected: FAIL - the token isn't in `CHECK_ERROR_TRANSLATIONS` yet, so `translateCheckError` returns it unchanged.

- [ ] **Step 3: Write the implementation**

In `bot/src/pingachock-client.ts`, replace:

```typescript
const CHECK_ERROR_TRANSLATIONS: Record<string, string> = {
  'no reply': 'нет ответа',
  timeout: 'таймаут',
  'dns resolution failed': 'домен не резолвится',
  'ping failed': 'ошибка проверки',
  'connection refused': 'соединение отклонено',
  'connection failed': 'не удалось подключиться',
  'certificate verification failed': 'ошибка проверки сертификата',
  // TLSChecker's supplementary ICMP probe (internal/checks/tls.go,
  // diagnoseUnreachable) - fired only for the ambiguous "timeout"/
  // "connection failed" buckets above, telling apart "whole host is down"
  // from "just this port is filtered, host answers pings fine".
  'ip unreachable': 'IP адрес недоступен (не отвечает на ping)',
  'port unreachable': 'порт недоступен (хост отвечает на ping)'
};
```

with:

```typescript
const CHECK_ERROR_TRANSLATIONS: Record<string, string> = {
  'no reply': 'нет ответа',
  timeout: 'таймаут',
  'dns resolution failed': 'домен не резолвится',
  'ping failed': 'ошибка проверки',
  'connection refused': 'соединение отклонено',
  'connection failed': 'не удалось подключиться',
  'certificate verification failed': 'ошибка проверки сертификата',
  // TLSChecker's supplementary ICMP probe (internal/checks/tls.go,
  // diagnoseUnreachable) - fired only for the ambiguous "timeout"/
  // "connection failed" buckets above, telling apart "whole host is down"
  // from "just this port is filtered, host answers pings fine".
  'ip unreachable': 'IP адрес недоступен (не отвечает на ping)',
  'port unreachable': 'порт недоступен (хост отвечает на ping)',
  // A node's operator-pinned interface disappeared mid-check (cable
  // unplugged, Wi-Fi off, adapter removed) - see
  // docs/superpowers/specs/2026-08-13-vpn-resilient-node-networking-design.md
  // Part 3. Distinct wording on purpose: this is an agent/host problem, not
  // a signal about the target's own reachability, so it shouldn't read like
  // every check on the node just started failing for censorship reasons.
  'network interface unavailable': 'сетевой интерфейс узла недоступен (нужно заново запустить configure)'
};
```

- [ ] **Step 4: Run test to verify it passes**

Run (from `bot/`): `npx tsx --test src/*.test.ts`
Expected: PASS - all bot tests except the 5 pre-existing `TEST_API_KEY`-gated ones in `pingachock-client.test.ts` (unrelated, unaffected by this change).

- [ ] **Step 5: Commit**

```bash
git add bot/src/pingachock-client.ts bot/src/icmp-summary.test.ts
git commit -m "bot: translate the new network-interface-unavailable classification token"
```

---

## Task 12: full verification and push

**Files:** none (verification only)

- [ ] **Step 1: Full Go build, all platforms**

Run:
```
go build ./...
GOOS=linux GOARCH=amd64 go build ./...
GOOS=linux GOARCH=arm64 go build ./...
GOOS=darwin GOARCH=amd64 go build ./...
GOOS=darwin GOARCH=arm64 go build ./...
GOOS=windows GOARCH=386 go build ./...
```
Expected: every one succeeds with no output. (Use the Bash tool's inline `VAR=x cmd` form for each, or PowerShell with `$env:GOOS`/`$env:GOARCH` set-then-reset around each call - see Task 2 Step 5's note. Reset both env vars to empty after the last one so nothing downstream of this step accidentally cross-compiles.)

- [ ] **Step 2: Full Go test suite**

Run: `go vet ./... && go test ./...`
Expected: `go vet` produces no output; every package reports `ok` (or `[no test files]` for packages that never had any - unchanged from before this plan).

- [ ] **Step 3: Full bot test suite**

Run (from `bot/`): `npx tsc -p tsconfig.json --noEmit && npx tsx --test src/*.test.ts`
Expected: `tsc` produces no output; the test run reports the same pass/fail split as before this plan (46 pass, 5 fail - the pre-existing `TEST_API_KEY`-gated live-backend tests - plus this plan's one new passing test from Task 11, so 47 pass / 5 fail).

- [ ] **Step 4: Manual `configure` + live check smoke test**

On this machine (Windows), with the agent already configured to a physical interface from Task 7's Step 3:

```
go run ./cmd/agent run
```

Let it poll at least once (or dispatch a `ping`/`tcp` check to this node from the bot/API if a live backend is reachable) and confirm in the agent's log output that checks complete normally - no new `"network interface unavailable"` errors, no crash. Stop the agent (Ctrl+C) once confirmed.

This step can't exercise "does binding actually route around a running VPN" without a real second network path/VPN client present in this environment - treat this smoke test as "did the rewritten NetConfig plumbing break anything observable," not as end-to-end proof of the VPN-bypass behavior itself; that part is a judgment call for the operator (the user) to confirm on their own node the next time a VPN happens to be running, per the design doc's whole motivating scenario.

- [ ] **Step 5: Push**

```bash
git push origin main
```

---

## Self-review notes (from writing this plan)

- **Spec coverage:** Part 1 (interface-identity binding) → Tasks 1-5, 7. Part 2 (VLESS `sendThrough`) → Task 6. Part 3 (interface-disappeared fails loud) → Tasks 1, 2, 3, 5, 6 all route through `ErrInterfaceUnavailable`/`"network interface unavailable"` uniformly. Part 4 (background self-test) → Tasks 8-10. Bot-side translation (mentioned in the design doc's Data Flow section implicitly, via "the bot can show the operator something actionable") → Task 11. Every design-doc section has at least one task.
- **Placeholder scan:** no TBD/TODO/"handle appropriately" phrasing anywhere above; every step shows complete, exact code.
- **Type consistency:** `checks.BindFunc` is defined once (Task 3) and reused verbatim (not redeclared) in `internal/netiface`'s per-OS files (Task 2 - those return the plain, structurally-identical func type, not `checks.BindFunc` itself, since `internal/netiface` must not import `internal/checks` - confirmed no import cycle either direction before writing this plan) and in `internal/poller.PathSelfTest.Bind` (Task 8, via `checks.BindFunc` directly, since `poller` already imports `checks`). `netiface.ByName`/`netiface.Interface`/`netiface.ErrInterfaceUnavailable` (Task 1) are used with matching names and signatures everywhere they're referenced later (Tasks 3, 5, 6, 7). `pingArgs`'s new `netiface.Interface` parameter (Task 5) matches at both of its two call sites (`ping.go`, `tls.go`).
- **Deliberately out of scope**, matching the design doc's own Non-goals: no change to how the agent reaches the backend itself; no bot UI changes beyond the one new translated string; no attempt to defeat true killswitch-level interception, only to stop it from producing fabricated data.
