// Package public implements the account-scoped public API (checks, nodes)
// consumed by external applications and the future web frontend.
package public

import (
	"time"

	"pingachock/internal/store"
)

type Handler struct {
	Store           *store.Store
	OnlineThreshold time.Duration
}

func New(s *store.Store, onlineThreshold time.Duration) *Handler {
	return &Handler{Store: s, OnlineThreshold: onlineThreshold}
}
