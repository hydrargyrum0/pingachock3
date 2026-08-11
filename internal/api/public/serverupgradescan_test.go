package public

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func doUpgradeScan(h *Handler, body any) (*httptest.ResponseRecorder, serverUpgradeScanResponse, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, serverUpgradeScanResponse{}, err
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/server-upgrade-scan", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	h.ServerUpgradeScan(rec, req)

	var resp serverUpgradeScanResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			return rec, serverUpgradeScanResponse{}, err
		}
	}
	return rec, resp, nil
}

// TestServerUpgradeScanReturnsOneResultPerTarget: the endpoint's request
// shape only takes bare targets (port 443 is always used, matching the
// design's fixed-port decision - see
// docs/superpowers/specs/2026-08-09-http-101-upgrade-check-design.md), so
// this test can't stand up a fake server to get matched:true (that would
// need binding port 443, privileged and untestable in CI) - the real
// matching logic is already fully covered by
// internal/checks/upgrade_test.go's TestUpgradeCheckerMatchesRealSwitchingProtocols.
// This layer's job is to verify the HTTP plumbing: one result per target,
// in order, with matched:false when nothing answers.
func TestServerUpgradeScanReturnsOneResultPerTarget(t *testing.T) {
	h := &Handler{}
	_, resp, err := doUpgradeScan(h, map[string]any{
		"targets": []string{"127.0.0.1", "203.0.113.1"}, // 203.0.113.1 is TEST-NET-3 (RFC 5737) - never routable
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(resp.Results))
	}
	for i, want := range []string{"127.0.0.1", "203.0.113.1"} {
		if resp.Results[i].Target != want {
			t.Errorf("Results[%d].Target = %q, want %q", i, resp.Results[i].Target, want)
		}
		if resp.Results[i].Matched {
			t.Errorf("Results[%d].Matched = true, want false - nothing is listening/routable", i)
		}
	}
}

// TestServerUpgradeScanConcurrentRequestsDoNotCrossTalk fires several
// concurrent requests, each about a *different* target, and checks each
// response only ever contains its own request's target - never another
// goroutine's. Same regression shape as serverping_test.go's
// TestServerPingConcurrentRequestsDoNotCrossTalk (the cross-talk bug
// described in docs/superpowers/specs/2026-07-19-telegram-bot-merge-design.md),
// adapted for this endpoint's fixed port: all targets are distinct loopback
// addresses (127.0.0.2 .. 127.0.0.9 - the entire 127.0.0.0/8 block is
// loopback, so nothing listens on port 443 on any of them, and the OS
// refuses the connection immediately without any real network round-trip).
func TestServerUpgradeScanConcurrentRequestsDoNotCrossTalk(t *testing.T) {
	const n = 8
	h := &Handler{}
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			target := fmt.Sprintf("127.0.0.%d", i+2)
			_, resp, err := doUpgradeScan(h, map[string]any{"targets": []string{target}})
			if err != nil {
				errs[i] = fmt.Errorf("goroutine %d: %w", i, err)
				return
			}
			if len(resp.Results) != 1 {
				errs[i] = fmt.Errorf("goroutine %d: got %d results, want 1", i, len(resp.Results))
				return
			}
			if resp.Results[0].Target != target {
				errs[i] = fmt.Errorf("goroutine %d: requested target %s, response target=%s - a different request's result leaked in", i, target, resp.Results[0].Target)
			}
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}

func TestServerUpgradeScanEmptyTargets(t *testing.T) {
	h := &Handler{}
	rec, _, err := doUpgradeScan(h, map[string]any{"targets": []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestServerUpgradeScanTooManyTargets(t *testing.T) {
	targets := make([]string, serverUpgradeScanMaxTargets+1)
	for i := range targets {
		targets[i] = "127.0.0.1"
	}
	h := &Handler{}
	rec, _, err := doUpgradeScan(h, map[string]any{"targets": targets})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}
