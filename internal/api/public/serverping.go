// ServerPing (POST /api/v1/server-ping) runs ICMP/TCP checks directly from
// this backend - no node involved, synchronous. See
// docs/superpowers/specs/2026-07-19-telegram-bot-merge-design.md.
package public

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"pingachock/internal/api"
	"pingachock/internal/checks"
)

const (
	serverPingMaxTargets = 50
	serverPingTimeout    = 20 * time.Second
)

type serverPingRequest struct {
	Targets []string `json:"targets"`
	Ports   []string `json:"ports"`
}

type serverPingICMPResult struct {
	Success   bool   `json:"success"`
	LatencyMs *int   `json:"latency_ms,omitempty"`
	Error     string `json:"error,omitempty"`
}

type serverPingTargetResult struct {
	Target     string                `json:"target"`
	ResolvedIP string                `json:"resolved_ip,omitempty"`
	ICMP       *serverPingICMPResult `json:"icmp,omitempty"`
	Ports      map[string]string     `json:"ports,omitempty"`
}

type serverPingResponse struct {
	Results []serverPingTargetResult `json:"results"`
}

// ServerPing does not touch h.Store: it never creates checks/check_runs, so
// every request is fully self-contained with zero state shared across
// concurrent requests. This is deliberate - see the spec's "Требование
// корректности" section, written after a real incident in a prior system
// where one user's ping response leaked into another's.
func (h *Handler) ServerPing(w http.ResponseWriter, r *http.Request) {
	var req serverPingRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if len(req.Targets) == 0 {
		api.WriteError(w, http.StatusBadRequest, "targets must not be empty")
		return
	}
	if len(req.Targets) > serverPingMaxTargets {
		api.WriteError(w, http.StatusBadRequest, fmt.Sprintf("targets: max %d per request", serverPingMaxTargets))
		return
	}

	ports := req.Ports
	if len(ports) == 0 {
		ports = []string{"icmp"}
	}
	for _, p := range ports {
		if p == "icmp" {
			continue
		}
		if _, err := strconv.Atoi(p); err != nil {
			api.WriteError(w, http.StatusBadRequest, "ports: each entry must be \"icmp\" or a numeric port, got "+p)
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), serverPingTimeout)
	defer cancel()

	results := make([]serverPingTargetResult, len(req.Targets))
	var wg sync.WaitGroup
	for i, target := range req.Targets {
		wg.Add(1)
		go func(i int, target string) {
			defer wg.Done()
			results[i] = runServerPingTarget(ctx, target, ports)
		}(i, target)
	}
	wg.Wait()

	api.WriteJSON(w, http.StatusOK, serverPingResponse{Results: results})
}

// runServerPingTarget runs every requested port check (plus ICMP, if asked
// for) against one target concurrently, and assembles them into one result.
// mu guards the shared `out` value - the WaitGroup in ServerPing already
// keeps different *targets* from touching each other's result slot, this
// mutex is only about the ports *within* a single target overlapping.
func runServerPingTarget(ctx context.Context, target string, ports []string) serverPingTargetResult {
	out := serverPingTargetResult{Target: target}
	var netCfg checks.NetConfig
	var wg sync.WaitGroup
	var mu sync.Mutex

	for _, p := range ports {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()

			if p == "icmp" {
				checker, _ := checks.Get("ping")
				res := checker.Run(ctx, netCfg, target, json.RawMessage(`{"count":1,"timeout_ms":3000}`))
				icmp := &serverPingICMPResult{Success: res.Success, LatencyMs: res.LatencyMs}
				if !res.Success && res.ErrorMessage != nil {
					icmp.Error = *res.ErrorMessage
				}
				mu.Lock()
				out.ICMP = icmp
				if out.ResolvedIP == "" {
					out.ResolvedIP = resolvedIPFromRaw(res.Raw)
				}
				mu.Unlock()
				return
			}

			checker, _ := checks.Get("tcp")
			// tcpParams.Port is an int, not a string - must convert before
			// marshaling. Getting this wrong (marshaling the raw string p
			// straight into {"port": p}) doesn't fail loudly: tcpParams'
			// own json.Unmarshal error is ignored by internal/checks/tcp.go,
			// so a type mismatch there silently leaves Port at its zero
			// value, which then defaults to 443 - every check would run
			// against port 443 regardless of what was actually requested.
			portNum, err := strconv.Atoi(p)
			if err != nil {
				return
			}
			params, _ := json.Marshal(map[string]any{"port": portNum})
			res := checker.Run(ctx, netCfg, target, params)
			status := "closed"
			if res.Success {
				status = "open"
			}
			mu.Lock()
			if out.Ports == nil {
				out.Ports = make(map[string]string)
			}
			out.Ports[p] = status
			mu.Unlock()
		}(p)
	}
	wg.Wait()
	return out
}

func resolvedIPFromRaw(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v struct {
		ResolvedTarget string `json:"resolved_target"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	return v.ResolvedTarget
}
