package checks

import (
	"context"
	"testing"
)

func TestResolveIPLiteralPassthrough(t *testing.T) {
	probeTarget, reportedIP := resolveIP(context.Background(), nil, "1.1.1.1")
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
	probeTarget, reportedIP := resolveIP(context.Background(), nil, "localhost")
	if probeTarget == "" || probeTarget == "localhost" {
		t.Fatalf("probeTarget = %q, want a resolved IP", probeTarget)
	}
	if reportedIP != probeTarget {
		t.Errorf("reportedIP = %q, want it to match probeTarget %q", reportedIP, probeTarget)
	}
}

func TestResolveIPUnresolvableFallsBackToOriginalTarget(t *testing.T) {
	const bogus = "this-domain-should-never-resolve.invalid"
	probeTarget, reportedIP := resolveIP(context.Background(), nil, bogus)
	if probeTarget != bogus {
		t.Errorf("probeTarget = %q, want the original target back so the caller still attempts something", probeTarget)
	}
	if reportedIP != "" {
		t.Errorf("reportedIP = %q, want \"\" on lookup failure", reportedIP)
	}
}
