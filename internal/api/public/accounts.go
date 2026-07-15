package public

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"pingachock/internal/api"
	"pingachock/internal/auth"
	"pingachock/internal/store"
)

type accountResponse struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

func toAccountResponse(a store.Account) accountResponse {
	return accountResponse{ID: a.ID, Name: a.Name, CreatedAt: a.CreatedAt}
}

type createAccountRequest struct {
	Name string `json:"name"`
}

// CreateAccount handles POST /accounts (admin-only).
func (h *Handler) CreateAccount(w http.ResponseWriter, r *http.Request) {
	var req createAccountRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Name == "" {
		api.WriteError(w, http.StatusBadRequest, "name is required")
		return
	}

	a, err := h.Store.CreateAccount(r.Context(), req.Name)
	if err != nil {
		api.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	api.WriteJSON(w, http.StatusCreated, toAccountResponse(a))
}

// ListAccounts handles GET /accounts (admin-only).
func (h *Handler) ListAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := h.Store.ListAccounts(r.Context())
	if err != nil {
		api.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]accountResponse, len(accounts))
	for i, a := range accounts {
		out[i] = toAccountResponse(a)
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

type apiKeyResponse struct {
	ID         uuid.UUID  `json:"id"`
	AccountID  uuid.UUID  `json:"account_id"`
	Label      string     `json:"label"`
	Scopes     []string   `json:"scopes"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

func toAPIKeyResponse(k store.APIKey) apiKeyResponse {
	return apiKeyResponse{
		ID: k.ID, AccountID: k.AccountID, Label: k.Label, Scopes: k.Scopes,
		CreatedAt: k.CreatedAt, LastUsedAt: k.LastUsedAt, RevokedAt: k.RevokedAt,
	}
}

type createAPIKeyRequest struct {
	Label  string   `json:"label"`
	Scopes []string `json:"scopes,omitempty"`
}

type createAPIKeyResponse struct {
	apiKeyResponse
	Token string `json:"token"`
}

// CreateAPIKey handles POST /accounts/{accountID}/api-keys (admin-only).
// Returns the token in plaintext exactly once - only its hash is stored,
// same pattern as node secrets (internal/auth).
func (h *Handler) CreateAPIKey(w http.ResponseWriter, r *http.Request) {
	accountID, err := uuid.Parse(chi.URLParam(r, "accountID"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "invalid account id")
		return
	}
	if _, err := h.Store.GetAccount(r.Context(), accountID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			api.WriteError(w, http.StatusNotFound, "account not found")
			return
		}
		api.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var req createAPIKeyRequest
	if r.ContentLength != 0 {
		if err := api.DecodeJSON(r, &req); err != nil {
			api.WriteError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}
	}

	token, err := auth.GenerateToken()
	if err != nil {
		api.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	k, err := h.Store.CreateAPIKey(r.Context(), accountID, auth.HashToken(token), req.Label, req.Scopes)
	if err != nil {
		api.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	api.WriteJSON(w, http.StatusCreated, createAPIKeyResponse{
		apiKeyResponse: toAPIKeyResponse(k),
		Token:          token,
	})
}

// ListAPIKeys handles GET /accounts/{accountID}/api-keys (admin-only). Never
// includes the token/hash - only metadata, for auditing what's issued.
func (h *Handler) ListAPIKeys(w http.ResponseWriter, r *http.Request) {
	accountID, err := uuid.Parse(chi.URLParam(r, "accountID"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "invalid account id")
		return
	}
	keys, err := h.Store.ListAPIKeysByAccount(r.Context(), accountID)
	if err != nil {
		api.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]apiKeyResponse, len(keys))
	for i, k := range keys {
		out[i] = toAPIKeyResponse(k)
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"api_keys": out})
}

// RevokeAPIKey handles DELETE /api-keys/{id} (admin-only).
func (h *Handler) RevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := h.Store.RevokeAPIKey(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			api.WriteError(w, http.StatusNotFound, "api key not found or already revoked")
			return
		}
		api.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
