// Package transport implements how a node talks to the backend: Direct
// (straight to the Debian server) and Fronted (via the Cloud Run reverse
// proxy, for when direct access is blocked). See docs/ARCHITECTURE.md.
package transport

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
)

type Job struct {
	CheckRunID uuid.UUID       `json:"check_run_id"`
	Type       string          `json:"type"`
	Target     string          `json:"target"`
	Params     json.RawMessage `json:"params"`
}

type ResultSubmission struct {
	CheckRunID   uuid.UUID       `json:"check_run_id"`
	Success      bool            `json:"success"`
	LatencyMs    *int            `json:"latency_ms,omitempty"`
	StatusCode   *string         `json:"status_code,omitempty"`
	ErrorMessage *string         `json:"error_message,omitempty"`
	Raw          json.RawMessage `json:"raw,omitempty"`
}

type Transport interface {
	Poll(ctx context.Context, agentVersion string) ([]Job, error)
	PostResults(ctx context.Context, results []ResultSubmission) error
}
