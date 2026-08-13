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
