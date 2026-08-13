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
