package public

import (
	"testing"

	"pingachock/internal/checks"
	"pingachock/internal/store"
)

// TestValidCheckTypesMatchesRegisteredCheckers guards against the exact
// bug this test was added for: internal/checks' own Checker registry
// (internal/checks/checks.go) and this handler's separate
// validCheckTypes allowlist are two independent lists that must be kept
// in sync by hand. "upgrade" and "vless" were registered as Checkers but
// never added here, so POST /api/v1/checks rejected them with 400
// "invalid type" before ever reaching dispatch - VLESS Speedtest was
// broken for every router, and HTTP 101 check was broken whenever a real
// node (not "server") was picked, since only the node-routed path goes
// through this validation at all.
func TestValidCheckTypesMatchesRegisteredCheckers(t *testing.T) {
	for _, ct := range []store.CheckType{
		store.CheckTypePing, store.CheckTypeTCP, store.CheckTypeHTTP, store.CheckTypeDNS,
		store.CheckTypeTLS, store.CheckTypeUpgrade, store.CheckTypeVless,
	} {
		if _, ok := checks.Get(string(ct)); !ok {
			t.Fatalf("checks.Get(%q) = false, ok - test fixture out of date, not a real bug", ct)
		}
		if !validCheckTypes[ct] {
			t.Errorf("validCheckTypes[%q] = false, want true - every registered checks.Checker must be reachable via POST /api/v1/checks", ct)
		}
	}
}
