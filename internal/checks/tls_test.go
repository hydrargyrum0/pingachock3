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
	"time"
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
//
// Targets the server by hostname ("localhost"), not its raw IP: a raw-IP
// target with no explicit SNI always skips verification (see
// tlsConfigFor's doc comment - there's no hostname to verify against in
// that case), so this test needs a real SNI in play to exercise actual
// certificate verification rather than that unrelated no-SNI path.
func TestTLSCheckerFailsCertVerificationByDefault(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	_, port := splitTestServerAddr(t, srv)

	params, _ := json.Marshal(map[string]any{"port": port, "count": 1})
	res := TLSChecker{}.Run(context.Background(), NetConfig{}, "localhost", params)

	if res.Success {
		t.Fatal("Success = true, want false - self-signed cert should fail verification without allow_insecure")
	}
	if res.ErrorMessage == nil {
		t.Fatal("ErrorMessage is nil, want a certificate verification error")
	}
}

// TestTLSCheckerRawIPWithoutSNISkipsVerification: dialing a raw IP with no
// explicit sni and no allow_insecure must still complete the handshake -
// this is the exact scenario TLSChecker's own doc comment claims to
// support ("no SNI sent - matches how a real client behaves connecting
// straight to an IP"), and Go's crypto/tls refuses to even attempt a
// handshake with an empty ServerName unless InsecureSkipVerify is set, so
// tlsConfigFor must set it automatically in this case.
func TestTLSCheckerRawIPWithoutSNISkipsVerification(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	host, port := splitTestServerAddr(t, srv)

	params, _ := json.Marshal(map[string]any{"port": port, "count": 1})
	res := TLSChecker{}.Run(context.Background(), NetConfig{}, host, params)

	if !res.Success {
		t.Fatalf("Success = false, want true (error: %v) - a raw-IP target with no SNI must skip verification, not be rejected by crypto/tls before any network I/O", res.ErrorMessage)
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

// TestTLSCheckerRequestsSentReflectsRealAttempts: when the parent context
// is cancelled partway through, requests_sent must report how many
// attempts actually happened, not the configured count - otherwise a run
// that tried once and succeeded gets misreported as e.g. "5 sent, 1
// succeeded" (80% failure) instead of "1 sent, 1 succeeded".
func TestTLSCheckerRequestsSentReflectsRealAttempts(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	host, port := splitTestServerAddr(t, srv)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled - the loop must not pretend p.Count attempts happened

	params, _ := json.Marshal(map[string]any{"port": port, "count": 5, "allow_insecure": true})
	res := TLSChecker{}.Run(ctx, NetConfig{}, host, params)

	var raw struct {
		RequestsSent int `json:"requests_sent"`
	}
	if err := json.Unmarshal(res.Raw, &raw); err != nil {
		t.Fatalf("unmarshal Raw: %v", err)
	}
	if raw.RequestsSent != 0 {
		t.Errorf("requests_sent=%d, want 0 - context was already cancelled before any attempt", raw.RequestsSent)
	}
}

// TestTLSCheckerHandshakeDeadline: a peer that accepts the TCP connection
// but never speaks TLS back must not hang the check forever - it has to
// give up around the configured timeout_ms, the same budget the TCP dial
// itself already respects.
func TestTLSCheckerHandshakeDeadline(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Accept the TCP connection and then just sit on it - never
			// send a single TLS byte back, simulating a stalling/censoring
			// middlebox post-SYN.
			_ = conn
		}
	}()

	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)

	params, _ := json.Marshal(map[string]any{"port": port, "count": 1, "timeout_ms": 300, "allow_insecure": true})

	start := time.Now()
	res := TLSChecker{}.Run(context.Background(), NetConfig{}, "127.0.0.1", params)
	elapsed := time.Since(start)

	if res.Success {
		t.Fatal("Success = true, want false - handshake never completed")
	}
	if elapsed > 3*time.Second {
		t.Errorf("Run() took %v, want it bounded by the ~300ms timeout_ms, not hanging indefinitely", elapsed)
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

func TestTLSConfigFor(t *testing.T) {
	cases := []struct {
		name          string
		sni           string
		allowInsecure bool
		wantInsecure  bool
	}{
		{"no SNI forces insecure regardless of the flag", "", false, true},
		{"no SNI, flag already true", "", true, true},
		{"SNI present, flag false -> real verification", "example.com", false, false},
		{"SNI present, flag true -> honors it", "example.com", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tlsConfigFor(tc.sni, tc.allowInsecure)
			if cfg.InsecureSkipVerify != tc.wantInsecure {
				t.Errorf("tlsConfigFor(%q, %v).InsecureSkipVerify = %v, want %v", tc.sni, tc.allowInsecure, cfg.InsecureSkipVerify, tc.wantInsecure)
			}
			if cfg.ServerName != tc.sni {
				t.Errorf("tlsConfigFor(%q, %v).ServerName = %q, want %q", tc.sni, tc.allowInsecure, cfg.ServerName, tc.sni)
			}
		})
	}
}
