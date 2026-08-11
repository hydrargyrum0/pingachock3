// ServerUpgradeScan (POST /api/v1/server-upgrade-scan) probes whether each
// target answers HTTP 101 Switching Protocols to a plaintext
// Connection: Upgrade request on port 443. Synchronous, no node involved,
// no DB access - same "every request is fully self-contained, no shared
// mutable state across concurrent requests" guarantee as ServerPing (see
// serverping.go's own doc comment). See
// docs/superpowers/specs/2026-08-09-http-101-upgrade-check-design.md.
package public

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"pingachock/internal/api"
	"pingachock/internal/checks"
)

const (
	serverUpgradeScanMaxTargets = 100
	serverUpgradeScanTimeout    = 20 * time.Second
)

type serverUpgradeScanRequest struct {
	Targets []string `json:"targets"`
}

type serverUpgradeScanResult struct {
	Target  string `json:"target"`
	Matched bool   `json:"matched"`
}

type serverUpgradeScanResponse struct {
	Results []serverUpgradeScanResult `json:"results"`
}

func (h *Handler) ServerUpgradeScan(w http.ResponseWriter, r *http.Request) {
	var req serverUpgradeScanRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if len(req.Targets) == 0 {
		api.WriteError(w, http.StatusBadRequest, "targets must not be empty")
		return
	}
	if len(req.Targets) > serverUpgradeScanMaxTargets {
		api.WriteError(w, http.StatusBadRequest, fmt.Sprintf("targets: max %d per request", serverUpgradeScanMaxTargets))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), serverUpgradeScanTimeout)
	defer cancel()

	checker, _ := checks.Get("upgrade")
	results := make([]serverUpgradeScanResult, len(req.Targets))
	var wg sync.WaitGroup
	for i, target := range req.Targets {
		wg.Add(1)
		go func(i int, target string) {
			defer wg.Done()
			res := checker.Run(ctx, checks.NetConfig{}, target, json.RawMessage(`{}`))
			results[i] = serverUpgradeScanResult{Target: target, Matched: res.Success}
		}(i, target)
	}
	wg.Wait()

	api.WriteJSON(w, http.StatusOK, serverUpgradeScanResponse{Results: results})
}
