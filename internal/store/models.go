package store

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

type Account struct {
	ID        uuid.UUID
	Name      string
	CreatedAt time.Time
}

type APIKey struct {
	ID         uuid.UUID
	AccountID  uuid.UUID
	KeyHash    string
	Label      string
	Scopes     []string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

type Node struct {
	ID              uuid.UUID
	Name            string
	ISP             string
	City            string
	Country         string
	AgentVersion    string
	LastHeartbeatAt *time.Time
	SecretHash      string
	Tags            json.RawMessage
	Metadata        json.RawMessage
	CreatedAt       time.Time
}

// Online reports whether the node has polled within threshold. There is no
// stored status column on purpose - see docs/ARCHITECTURE.md.
func (n Node) Online(threshold time.Duration) bool {
	if n.LastHeartbeatAt == nil {
		return false
	}
	return time.Since(*n.LastHeartbeatAt) <= threshold
}

type CheckType string

const (
	CheckTypePing       CheckType = "ping"
	CheckTypeTCP        CheckType = "tcp"
	CheckTypeHTTP       CheckType = "http"
	CheckTypeDNS        CheckType = "dns"
	CheckTypeTLS        CheckType = "tls"
	CheckTypeTraceroute CheckType = "traceroute"
)

type CheckStatus string

const (
	CheckStatusPending   CheckStatus = "pending"
	CheckStatusRunning   CheckStatus = "running"
	CheckStatusCompleted CheckStatus = "completed"
	CheckStatusPartial   CheckStatus = "partial"
	CheckStatusFailed    CheckStatus = "failed"
	CheckStatusCancelled CheckStatus = "cancelled"
)

type Check struct {
	ID           uuid.UUID
	AccountID    uuid.UUID
	BatchID      *uuid.UUID
	Type         CheckType
	Target       string
	Params       json.RawMessage
	NodeSelector json.RawMessage
	CallbackURL  *string
	Status       CheckStatus
	Warnings     []string
	CreatedAt    time.Time
	CompletedAt  *time.Time
}

type CheckRunStatus string

const (
	CheckRunStatusQueued     CheckRunStatus = "queued"
	CheckRunStatusDispatched CheckRunStatus = "dispatched"
	CheckRunStatusRunning    CheckRunStatus = "running"
	CheckRunStatusDone       CheckRunStatus = "done"
	CheckRunStatusError      CheckRunStatus = "error"
	CheckRunStatusTimeout    CheckRunStatus = "timeout"
)

type CheckRun struct {
	ID           uuid.UUID
	CheckID      uuid.UUID
	NodeID       uuid.UUID
	Status       CheckRunStatus
	DispatchedAt *time.Time
	CompletedAt  *time.Time
	CreatedAt    time.Time
}

// CheckRunJob is what a node receives from /agent/poll: the run plus enough
// of the parent check to actually execute it.
type CheckRunJob struct {
	CheckRunID uuid.UUID
	CheckID    uuid.UUID
	Type       CheckType
	Target     string
	Params     json.RawMessage
}

type Result struct {
	ID           uuid.UUID
	CheckRunID   uuid.UUID
	Success      bool
	LatencyMs    *int
	StatusCode   *string
	ErrorMessage *string
	Raw          json.RawMessage
	CreatedAt    time.Time
}

// RunWithResult is a check_run joined with its node and (if present) result -
// the shape needed for GET /checks/{id}?expand=runs.
type RunWithResult struct {
	Run    CheckRun
	Node   Node
	Result *Result
}
