package checks

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// splitTestServerAddr pulls the host and numeric port back out of an
// httptest server's URL, since TLSChecker.Run takes them separately (target
// + params.port, mirroring every other checker in this package).
func splitTestServerAddr(t *testing.T, srv *httptest.Server) (host string, port int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(strings.TrimPrefix(strings.TrimPrefix(srv.URL, "https://"), "http://"))
	if err != nil {
		t.Fatalf("split server addr %q: %v", srv.URL, err)
	}
	port, err = strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port %q: %v", portStr, err)
	}
	return host, port
}

func TestTLSCheckerSucceedsWithAllowInsecure(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	host, port := splitTestServerAddr(t, srv)

	params, _ := json.Marshal(map[string]any{"port": port, "count": 3, "allow_insecure": true})
	res := TLSChecker{}.Run(context.Background(), NetConfig{}, host, params)

	if !res.Success {
		t.Fatalf("Success = false, want true (error: %v)", res.ErrorMessage)
	}
	if res.LatencyMs == nil {
		t.Fatal("LatencyMs is nil, want a real handshake duration")
	}

	var raw struct {
		RequestsSent    int `json:"requests_sent"`
		RequestsSuccess int `json:"requests_success"`
	}
	if err := json.Unmarshal(res.Raw, &raw); err != nil {
		t.Fatalf("unmarshal Raw: %v", err)
	}
	if raw.RequestsSent != 3 || raw.RequestsSuccess != 3 {
		t.Errorf("requests_sent=%d requests_success=%d, want 3/3", raw.RequestsSent, raw.RequestsSuccess)
	}
}

// TestTLSCheckerFailsCertVerificationByDefault: the self-signed cert
// httptest.NewTLSServer hands out isn't trusted by anything, so without
// allow_insecure the handshake must fail verification - this is the
// documented, deliberate default (see AllowInsecure's doc comment), not a
// bug, and it's the one thing that distinguishes it from a plain TCP check.
func TestTLSCheckerFailsCertVerificationByDefault(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	host, port := splitTestServerAddr(t, srv)

	params, _ := json.Marshal(map[string]any{"port": port, "count": 1})
	res := TLSChecker{}.Run(context.Background(), NetConfig{}, host, params)

	if res.Success {
		t.Fatal("Success = true, want false - self-signed cert should fail verification without allow_insecure")
	}
	if res.ErrorMessage == nil {
		t.Fatal("ErrorMessage is nil, want a certificate verification error")
	}
}

func TestTLSCheckerConnectionRefusedFailsAllAttempts(t *testing.T) {
	// Port 1 is reserved (tcpmux) and nothing should ever be listening on
	// it - dial should fail fast and consistently across environments.
	params, _ := json.Marshal(map[string]any{"port": 1, "count": 2, "timeout_ms": 1000})
	res := TLSChecker{}.Run(context.Background(), NetConfig{}, "127.0.0.1", params)

	if res.Success {
		t.Fatal("Success = true, want false - nothing is listening")
	}
	if res.LatencyMs != nil {
		t.Errorf("LatencyMs = %v, want nil - no handshake ever completed", *res.LatencyMs)
	}

	var raw struct {
		RequestsSent    int `json:"requests_sent"`
		RequestsSuccess int `json:"requests_success"`
	}
	if err := json.Unmarshal(res.Raw, &raw); err != nil {
		t.Fatalf("unmarshal Raw: %v", err)
	}
	if raw.RequestsSent != 2 || raw.RequestsSuccess != 0 {
		t.Errorf("requests_sent=%d requests_success=%d, want 2/0", raw.RequestsSent, raw.RequestsSuccess)
	}
}

func TestChooseSNI(t *testing.T) {
	cases := []struct {
		name     string
		target   string
		explicit string
		want     string
	}{
		{name: "domain target, no explicit SNI -> defaults to target", target: "example.com", explicit: "", want: "example.com"},
		{name: "IP target, no explicit SNI -> no SNI sent", target: "203.0.113.5", explicit: "", want: ""},
		{name: "IP target, explicit SNI -> uses it (the Cloudflare-fronting case)", target: "203.0.113.5", explicit: "front.example", want: "front.example"},
		{name: "domain target, explicit SNI overrides the target", target: "example.com", explicit: "front.example", want: "front.example"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := chooseSNI(tc.target, tc.explicit); got != tc.want {
				t.Errorf("chooseSNI(%q, %q) = %q, want %q", tc.target, tc.explicit, got, tc.want)
			}
		})
	}
}
