package public

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"pingachock/internal/api"
	"pingachock/internal/auth"
	"pingachock/internal/store"
)

type nodeResponse struct {
	ID           uuid.UUID       `json:"id"`
	Name         string          `json:"name"`
	ISP          string          `json:"isp"`
	City         string          `json:"city"`
	Country      string          `json:"country"`
	AgentVersion string          `json:"agent_version"`
	Platform     string          `json:"platform,omitempty"`
	Online       bool            `json:"online"`
	Blocked      bool            `json:"blocked"`
	LastSeenAt   *time.Time      `json:"last_seen_at,omitempty"`
	Tags         json.RawMessage `json:"tags"`
	CreatedAt    time.Time       `json:"created_at"`
}

func toNodeResponse(n store.Node, threshold time.Duration) nodeResponse {
	return nodeResponse{
		ID: n.ID, Name: n.Name, ISP: n.ISP, City: n.City, Country: n.Country,
		AgentVersion: n.AgentVersion, Platform: n.Platform, Online: n.Online(threshold), Blocked: n.Blocked,
		LastSeenAt: n.LastHeartbeatAt, Tags: n.Tags, CreatedAt: n.CreatedAt,
	}
}

func (h *Handler) ListNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := h.Store.ListNodes(r.Context())
	if err != nil {
		api.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]nodeResponse, len(nodes))
	for i, n := range nodes {
		out[i] = toNodeResponse(n, h.OnlineThreshold)
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"nodes": out})
}

func (h *Handler) GetNode(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "invalid id")
		return
	}
	n, err := h.Store.GetNode(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		api.WriteError(w, http.StatusNotFound, "node not found")
		return
	}
	if err != nil {
		api.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	api.WriteJSON(w, http.StatusOK, toNodeResponse(n, h.OnlineThreshold))
}

type createNodeRequest struct {
	Name string `json:"name"`
	ISP  string `json:"isp"`
	City string `json:"city"`
}

type createNodeResponse struct {
	nodeResponse
	Secret string `json:"secret"`
}

// CreateNode handles POST /nodes (admin-only). Returns the node's secret in
// plaintext exactly once - only its hash is stored.
func (h *Handler) CreateNode(w http.ResponseWriter, r *http.Request) {
	var req createNodeRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Name == "" {
		api.WriteError(w, http.StatusBadRequest, "name is required")
		return
	}

	secret, err := auth.GenerateToken()
	if err != nil {
		api.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	n, err := h.Store.CreateNode(r.Context(), req.Name, req.ISP, req.City, auth.HashToken(secret))
	if err != nil {
		api.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	api.WriteJSON(w, http.StatusCreated, createNodeResponse{
		nodeResponse: toNodeResponse(n, h.OnlineThreshold),
		Secret:       secret,
	})
}

type updateNodeRequest struct {
	Blocked *bool `json:"blocked,omitempty"`
}

// UpdateNode handles PUT /nodes/{id} (admin-only). Currently only supports
// toggling `blocked`: excludes the node from new dispatch (see
// internal/dispatch) without deleting it, so its check history is kept.
func (h *Handler) UpdateNode(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var req updateNodeRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Blocked == nil {
		api.WriteError(w, http.StatusBadRequest, "blocked is required")
		return
	}
	if err := h.Store.SetNodeBlocked(r.Context(), id, *req.Blocked); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			api.WriteError(w, http.StatusNotFound, "node not found")
			return
		}
		api.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	n, err := h.Store.GetNode(r.Context(), id)
	if err != nil {
		api.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	api.WriteJSON(w, http.StatusOK, toNodeResponse(n, h.OnlineThreshold))
}
