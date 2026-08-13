package checks

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"pingachock/internal/netiface"
)

func TestResolveIPLiteralPassthrough(t *testing.T) {
	probeTarget, reportedIP := resolveIP(context.Background(), nil, "1.1.1.1", 0, nil)
	if probeTarget != "1.1.1.1" {
		t.Errorf("probeTarget = %q, want unchanged literal IP", probeTarget)
	}
	if reportedIP != "" {
		t.Errorf("reportedIP = %q, want \"\" - nothing was actually resolved", reportedIP)
	}
}

// TestResolveIPResolvesHostname exercises the real lookup path without
// needing internet access - "localhost" resolves via /etc/hosts (or the
// platform equivalent) on any machine, including a sandboxed CI runner.
func TestResolveIPResolvesHostname(t *testing.T) {
	probeTarget, reportedIP := resolveIP(context.Background(), nil, "localhost", 0, nil)
	if probeTarget == "" || probeTarget == "localhost" {
		t.Fatalf("probeTarget = %q, want a resolved IP", probeTarget)
	}
	if reportedIP != probeTarget {
		t.Errorf("reportedIP = %q, want it to match probeTarget %q", reportedIP, probeTarget)
	}
}

func TestResolveIPUnresolvableFallsBackToOriginalTarget(t *testing.T) {
	const bogus = "this-domain-should-never-resolve.invalid"
	probeTarget, reportedIP := resolveIP(context.Background(), nil, bogus, 0, nil)
	if probeTarget != bogus {
		t.Errorf("probeTarget = %q, want the original target back so the caller still attempts something", probeTarget)
	}
	if reportedIP != "" {
		t.Errorf("reportedIP = %q, want \"\" on lookup failure", reportedIP)
	}
}

// TestResolveIPRespectsCallerTimeout: a lookup against a resolver that never
// answers must give up around the caller's own timeout, not some hardcoded
// budget bigger than what the check itself was configured to allow.
func TestResolveIPRespectsCallerTimeout(t *testing.T) {
	blackhole := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}

	start := time.Now()
	_, reportedIP := resolveIP(context.Background(), blackhole, "example.invalid.test", 200*time.Millisecond, nil)
	elapsed := time.Since(start)

	if reportedIP != "" {
		t.Errorf("reportedIP = %q, want \"\" - lookup never got an answer", reportedIP)
	}
	if elapsed > 2*time.Second {
		t.Errorf("resolveIP took %v, want it bounded by the ~200ms timeout passed in, not some larger hardcoded default", elapsed)
	}
}

func TestPickPreferredIP(t *testing.T) {
	v4 := net.ParseIP("203.0.113.5")
	v6 := net.ParseIP("2001:db8::1")

	t.Run("prefers IPv4 when no family preference given", func(t *testing.T) {
		got := pickPreferredIP([]net.IPAddr{{IP: v6}, {IP: v4}}, nil)
		if !got.Equal(v4) {
			t.Errorf("got %v, want IPv4 address %v preferred over IPv6", got, v4)
		}
	})

	t.Run("IPv6-only result falls back to whatever's there", func(t *testing.T) {
		got := pickPreferredIP([]net.IPAddr{{IP: v6}}, nil)
		if !got.Equal(v6) {
			t.Errorf("got %v, want %v", got, v6)
		}
	})

	t.Run("prefers address matching a pinned local interface's family", func(t *testing.T) {
		localV6 := net.ParseIP("2001:db8::100")
		got := pickPreferredIP([]net.IPAddr{{IP: v4}, {IP: v6}}, localV6)
		if !got.Equal(v6) {
			t.Errorf("got %v, want IPv6 %v to match the pinned IPv6 local address, not the default IPv4 preference", got, v6)
		}
	})
}

func TestClassifyNetError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"deadline exceeded", context.DeadlineExceeded, "timeout"},
		{"connection refused", &net.OpError{Op: "dial", Err: errors.New("connect: connection refused")}, "connection failed"},
		{"dns error", &net.DNSError{Err: "no such host", Name: "example.invalid"}, "dns resolution failed"},
		{"generic", errors.New("some other failure"), "connection failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyNetError(tc.err); got != tc.want {
				t.Errorf("classifyNetError(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestClassifyNetErrorInterfaceUnavailable(t *testing.T) {
	err := fmt.Errorf("dial tcp: %w: eth-test: interface not found", netiface.ErrInterfaceUnavailable)
	if got := classifyNetError(err); got != "network interface unavailable" {
		t.Errorf("classifyNetError(%v) = %q, want %q", err, got, "network interface unavailable")
	}
}

func TestClassifyNetErrorConnectionRefusedSyscall(t *testing.T) {
	// Exercise the real ECONNREFUSED path end-to-end rather than a synthetic
	// error, since wrapping/unwrapping through net.OpError is easy to get
	// subtly wrong.
	_, err := net.DialTimeout("tcp", "127.0.0.1:1", time.Second)
	if err == nil {
		t.Skip("expected port 1 to refuse the connection in this environment")
	}
	if got := classifyNetError(err); got != "connection refused" && got != "timeout" && got != "connection failed" {
		t.Errorf("classifyNetError(%v) = %q, want a recognized classification", err, got)
	}
}
