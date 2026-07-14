package auth

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"pingachock/internal/store"
)

type ctxKey int

const (
	accountCtxKey ctxKey = iota
	nodeCtxKey
)

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}
	return strings.TrimPrefix(h, prefix), true
}

// RequireAPIKey authenticates public API requests via
// `Authorization: Bearer <api_key>` and puts the owning account ID in the
// request context.
func RequireAPIKey(s *store.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r)
			if !ok {
				writeUnauthorized(w, "missing bearer token")
				return
			}
			key, err := s.GetAPIKeyByHash(r.Context(), HashToken(token))
			if err != nil {
				writeUnauthorized(w, "invalid api key")
				return
			}
			_ = s.TouchAPIKeyLastUsed(r.Context(), key.ID)
			ctx := context.WithValue(r.Context(), accountCtxKey, key.AccountID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireNodeSecret authenticates agent-protocol requests via
// `Authorization: Bearer <node_secret>` and puts the node ID in the request
// context. Deliberately a separate credential space from api_key - a node
// should never be able to call the public API with its own secret.
func RequireNodeSecret(s *store.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r)
			if !ok {
				writeUnauthorized(w, "missing bearer token")
				return
			}
			node, err := s.GetNodeBySecretHash(r.Context(), HashToken(token))
			if err != nil {
				writeUnauthorized(w, "invalid node secret")
				return
			}
			ctx := context.WithValue(r.Context(), nodeCtxKey, node.ID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireAdminToken gates operator-only endpoints (like provisioning a new
// node) behind a single shared secret configured on the server - these
// aren't exposed as self-serve, account-scoped API surface (yet).
func RequireAdminToken(adminToken string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r)
			if !ok || subtle.ConstantTimeCompare([]byte(token), []byte(adminToken)) != 1 {
				writeUnauthorized(w, "invalid admin token")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func writeUnauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	w.Write([]byte(`{"error":"` + msg + `"}`))
}

func AccountID(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(accountCtxKey).(uuid.UUID)
	return id, ok
}

func NodeID(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(nodeCtxKey).(uuid.UUID)
	return id, ok
}
