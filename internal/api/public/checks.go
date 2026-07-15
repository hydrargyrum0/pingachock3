package public

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"pingachock/internal/api"
	"pingachock/internal/auth"
	"pingachock/internal/dispatch"
	"pingachock/internal/store"
)

var validCheckTypes = map[store.CheckType]bool{
	store.CheckTypePing:       true,
	store.CheckTypeTCP:        true,
	store.CheckTypeHTTP:       true,
	store.CheckTypeDNS:        true,
	store.CheckTypeTLS:        true,
	store.CheckTypeTraceroute: true,
}

type createCheckRequest struct {
	Type         store.CheckType       `json:"type"`
	Target       string                `json:"target,omitempty"`
	Targets      []string              `json:"targets,omitempty"`
	Params       json.RawMessage       `json:"params,omitempty"`
	NodeSelector dispatch.NodeSelector `json:"node_selector"`
	CallbackURL  *string               `json:"callback_url,omitempty"`
}

type checkResponse struct {
	ID           uuid.UUID         `json:"id"`
	BatchID      *uuid.UUID        `json:"batch_id,omitempty"`
	Type         store.CheckType   `json:"type"`
	Target       string            `json:"target"`
	Params       json.RawMessage   `json:"params"`
	NodeSelector json.RawMessage   `json:"node_selector"`
	CallbackURL  *string           `json:"callback_url,omitempty"`
	Status       store.CheckStatus `json:"status"`
	Warnings     []string          `json:"warnings,omitempty"`
	CreatedAt    time.Time         `json:"created_at"`
	CompletedAt  *time.Time        `json:"completed_at,omitempty"`
	Runs         []runResponse     `json:"runs,omitempty"`
}

type runResponse struct {
	ID           uuid.UUID            `json:"id"`
	NodeID       uuid.UUID            `json:"node_id"`
	NodeName     string               `json:"node_name"`
	Status       store.CheckRunStatus `json:"status"`
	DispatchedAt *time.Time           `json:"dispatched_at,omitempty"`
	CompletedAt  *time.Time           `json:"completed_at,omitempty"`
	Result       *resultResponse      `json:"result,omitempty"`
}

type resultResponse struct {
	Success      bool            `json:"success"`
	LatencyMs    *int            `json:"latency_ms,omitempty"`
	StatusCode   *string         `json:"status_code,omitempty"`
	ErrorMessage *string         `json:"error_message,omitempty"`
	Raw          json.RawMessage `json:"raw,omitempty"`
}

func toCheckResponse(c store.Check) checkResponse {
	return checkResponse{
		ID: c.ID, BatchID: c.BatchID, Type: c.Type, Target: c.Target,
		Params: c.Params, NodeSelector: c.NodeSelector, CallbackURL: c.CallbackURL,
		Status: c.Status, Warnings: c.Warnings, CreatedAt: c.CreatedAt, CompletedAt: c.CompletedAt,
	}
}

func toRunResponse(rw store.RunWithResult) runResponse {
	out := runResponse{
		ID: rw.Run.ID, NodeID: rw.Node.ID, NodeName: rw.Node.Name, Status: rw.Run.Status,
		DispatchedAt: rw.Run.DispatchedAt, CompletedAt: rw.Run.CompletedAt,
	}
	if rw.Result != nil {
		out.Result = &resultResponse{
			Success: rw.Result.Success, LatencyMs: rw.Result.LatencyMs,
			StatusCode: rw.Result.StatusCode, ErrorMessage: rw.Result.ErrorMessage, Raw: rw.Result.Raw,
		}
	}
	return out
}

// CreateCheck handles POST /checks. A single `target` creates one check; a
// `targets` array fans out one check per target sharing a batch_id (see
// docs/ARCHITECTURE.md "Батч-пинг").
func (h *Handler) CreateCheck(w http.ResponseWriter, r *http.Request) {
	accountID, _ := auth.AccountID(r.Context())

	var req createCheckRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if !validCheckTypes[req.Type] {
		api.WriteError(w, http.StatusBadRequest, "invalid type")
		return
	}

	targets := append([]string{}, req.Targets...)
	if req.Target != "" {
		targets = append(targets, req.Target)
	}
	if len(targets) == 0 {
		api.WriteError(w, http.StatusBadRequest, "must specify target or targets")
		return
	}

	params := req.Params
	if len(params) == 0 {
		params = json.RawMessage(`{}`)
	}
	selectorJSON, err := json.Marshal(req.NodeSelector)
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "invalid node_selector")
		return
	}

	nodeIDs, warnings, err := dispatch.Resolve(r.Context(), h.Store, req.NodeSelector, h.OnlineThreshold)
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(nodeIDs) == 0 {
		api.WriteError(w, http.StatusUnprocessableEntity, "node_selector matched no available nodes")
		return
	}

	var batchID *uuid.UUID
	if len(targets) > 1 {
		id := uuid.New()
		batchID = &id
	}

	checks := make([]store.Check, 0, len(targets))
	for _, target := range targets {
		c, err := h.Store.CreateCheck(r.Context(), store.CreateCheckParams{
			AccountID: accountID, BatchID: batchID, Type: req.Type, Target: target,
			Params: params, NodeSelector: selectorJSON, CallbackURL: req.CallbackURL,
		})
		if err != nil {
			api.WriteError(w, http.StatusInternalServerError, "create check: "+err.Error())
			return
		}
		if len(warnings) > 0 {
			if err := h.Store.SetCheckWarnings(r.Context(), c.ID, warnings); err != nil {
				api.WriteError(w, http.StatusInternalServerError, "set warnings: "+err.Error())
				return
			}
			c.Warnings = warnings
		}
		if _, err := h.Store.CreateCheckRuns(r.Context(), c.ID, nodeIDs); err != nil {
			api.WriteError(w, http.StatusInternalServerError, "create check_runs: "+err.Error())
			return
		}
		checks = append(checks, c)
	}

	if batchID == nil {
		api.WriteJSON(w, http.StatusCreated, toCheckResponse(checks[0]))
		return
	}
	resp := struct {
		BatchID uuid.UUID       `json:"batch_id"`
		Checks  []checkResponse `json:"checks"`
	}{BatchID: *batchID, Checks: make([]checkResponse, len(checks))}
	for i, c := range checks {
		resp.Checks[i] = toCheckResponse(c)
	}
	api.WriteJSON(w, http.StatusCreated, resp)
}

func (h *Handler) GetCheck(w http.ResponseWriter, r *http.Request) {
	accountID, _ := auth.AccountID(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "invalid id")
		return
	}

	c, err := h.Store.GetCheck(r.Context(), accountID, id)
	if errors.Is(err, store.ErrNotFound) {
		api.WriteError(w, http.StatusNotFound, "check not found")
		return
	}
	if err != nil {
		api.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	resp := toCheckResponse(c)
	if r.URL.Query().Get("expand") == "runs" {
		runs, err := h.Store.ListRunsForCheck(r.Context(), c.ID)
		if err != nil {
			api.WriteError(w, http.StatusInternalServerError, err.Error())
			return
		}
		resp.Runs = make([]runResponse, len(runs))
		for i, rw := range runs {
			resp.Runs[i] = toRunResponse(rw)
		}
	}
	api.WriteJSON(w, http.StatusOK, resp)
}

func (h *Handler) ListChecks(w http.ResponseWriter, r *http.Request) {
	accountID, _ := auth.AccountID(r.Context())
	f := store.ListChecksFilter{AccountID: accountID}

	q := r.URL.Query()
	if bid := q.Get("batch_id"); bid != "" {
		id, err := uuid.Parse(bid)
		if err != nil {
			api.WriteError(w, http.StatusBadRequest, "invalid batch_id")
			return
		}
		f.BatchID = &id
	}
	if st := q.Get("status"); st != "" {
		s := store.CheckStatus(st)
		f.Status = &s
	}
	if l := q.Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil {
			f.Limit = n
		}
	}
	if o := q.Get("offset"); o != "" {
		if n, err := strconv.Atoi(o); err == nil {
			f.Offset = n
		}
	}

	checks, err := h.Store.ListChecks(r.Context(), f)
	if err != nil {
		api.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]checkResponse, len(checks))
	for i, c := range checks {
		out[i] = toCheckResponse(c)
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"checks": out})
}

func (h *Handler) CancelCheck(w http.ResponseWriter, r *http.Request) {
	accountID, _ := auth.AccountID(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "invalid id")
		return
	}

	if err := h.Store.CancelCheck(r.Context(), accountID, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			api.WriteError(w, http.StatusNotFound, "check not found or already finished")
			return
		}
		api.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
