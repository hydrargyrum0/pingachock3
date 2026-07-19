// Package agent implements the two-endpoint node protocol: poll for jobs,
// submit results. See docs/ARCHITECTURE.md "Протокол агента".
package agent

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"pingachock/internal/api"
	"pingachock/internal/auth"
	"pingachock/internal/store"
)

type Handler struct {
	Store          *store.Store
	PollBatchLimit int
}

func New(s *store.Store, pollBatchLimit int) *Handler {
	return &Handler{Store: s, PollBatchLimit: pollBatchLimit}
}

type pollRequest struct {
	AgentVersion string `json:"agent_version,omitempty"`
	Platform     string `json:"platform,omitempty"`
}

type pollJob struct {
	CheckRunID uuid.UUID       `json:"check_run_id"`
	Type       store.CheckType `json:"type"`
	Target     string          `json:"target"`
	Params     json.RawMessage `json:"params"`
}

type pollResponse struct {
	Jobs []pollJob `json:"jobs"`
}

// Poll doubles as the node's heartbeat: every call updates last_heartbeat_at
// regardless of whether there's work, then claims up to PollBatchLimit
// queued check_runs for this node.
func (h *Handler) Poll(w http.ResponseWriter, r *http.Request) {
	nodeID, _ := auth.NodeID(r.Context())

	var req pollRequest
	if r.ContentLength != 0 {
		if err := api.DecodeJSON(r, &req); err != nil {
			api.WriteError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}
	}

	if err := h.Store.TouchHeartbeat(r.Context(), nodeID); err != nil {
		api.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if req.AgentVersion != "" {
		_ = h.Store.SetNodeAgentVersion(r.Context(), nodeID, req.AgentVersion)
	}
	if req.Platform != "" {
		_ = h.Store.SetNodePlatform(r.Context(), nodeID, req.Platform)
	}

	jobs, err := h.Store.ClaimQueuedRuns(r.Context(), nodeID, h.PollBatchLimit)
	if err != nil {
		api.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	resp := pollResponse{Jobs: make([]pollJob, len(jobs))}
	for i, j := range jobs {
		resp.Jobs[i] = pollJob{CheckRunID: j.CheckRunID, Type: j.Type, Target: j.Target, Params: j.Params}
	}
	api.WriteJSON(w, http.StatusOK, resp)
}

type resultSubmission struct {
	CheckRunID   uuid.UUID       `json:"check_run_id"`
	Success      bool            `json:"success"`
	LatencyMs    *int            `json:"latency_ms,omitempty"`
	StatusCode   *string         `json:"status_code,omitempty"`
	ErrorMessage *string         `json:"error_message,omitempty"`
	Raw          json.RawMessage `json:"raw,omitempty"`
}

type resultsRequest struct {
	Results []resultSubmission `json:"results"`
}

// Results accepts a batch of results in one call - a node that picked up
// several check_runs from one poll reports them all in one POST.
func (h *Handler) Results(w http.ResponseWriter, r *http.Request) {
	nodeID, _ := auth.NodeID(r.Context())

	var req resultsRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if len(req.Results) == 0 {
		api.WriteError(w, http.StatusBadRequest, "results must not be empty")
		return
	}

	for _, res := range req.Results {
		raw := res.Raw
		if len(raw) == 0 {
			raw = json.RawMessage(`{}`)
		}
		err := h.Store.CompleteCheckRun(r.Context(), res.CheckRunID, nodeID, res.Success, res.LatencyMs, res.StatusCode, res.ErrorMessage, raw)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				api.WriteError(w, http.StatusNotFound, "check_run not found or not assigned to this node: "+res.CheckRunID.String())
				return
			}
			api.WriteError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
