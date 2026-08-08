// Package serveragent lets the backend execute checks against itself
// through the exact same check_run dispatch machinery real remote agents
// use, instead of a separate hand-rolled endpoint per supported check type.
// POST /api/v1/checks with node_selector targeting the virtual "server"
// node works for every check type in internal/checks' registry - the same
// generic path any real node already goes through - not just the fixed
// icmp/tcp subset /api/v1/server-ping hard-codes. See docs/ARCHITECTURE.md.
package serveragent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"pingachock/internal/auth"
	"pingachock/internal/checks"
	"pingachock/internal/store"
)

// NodeName is the fixed name the virtual node is provisioned under - what
// shows up as this "node"'s name in GET /api/v1/nodes.
const NodeName = "server"

// Ensure idempotently provisions the singleton virtual "server" node - safe
// to call on every boot, including by two backend instances booting at the
// same time (a rolling deploy or horizontal scale-up, which
// docs/ARCHITECTURE.md explicitly documents as needing nothing extra). Its
// secret is generated but never actually used for auth: unlike a real
// agent, the virtual node is never reached over HTTP (see Runner), it's
// just a placeholder to satisfy nodes.secret_hash's NOT NULL constraint.
func Ensure(ctx context.Context, st *store.Store) (store.Node, error) {
	n, err := st.GetVirtualNode(ctx)
	if err == nil {
		return n, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return store.Node{}, err
	}
	secret, err := auth.GenerateToken()
	if err != nil {
		return store.Node{}, fmt.Errorf("generate virtual node secret: %w", err)
	}
	n, err = st.CreateVirtualNode(ctx, NodeName, auth.HashToken(secret))
	if err != nil {
		if isUniqueViolation(err) {
			// Lost the boot-time race: another instance's CreateVirtualNode
			// won first (idx_nodes_one_virtual, migrations/0003). Not a
			// real failure - read back the winner's row instead of
			// crash-looping this instance.
			return st.GetVirtualNode(ctx)
		}
		return store.Node{}, err
	}
	return n, nil
}

// isUniqueViolation reports whether err is Postgres' unique_violation
// (SQLSTATE 23505) - errors.As works through however many layers
// database/sql and the pgx stdlib driver wrap the underlying *pgconn.PgError
// in.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// Runner polls its own store for check_runs assigned to the virtual server
// node and executes them in-process - no HTTP hop between "poll" and
// "execute" the way a remote agent has, so the interval can be short (this
// is what stands in for the old /api/v1/server-ping's synchronous feel).
type Runner struct {
	Store  *store.Store
	NodeID uuid.UUID
	// Interval is how often tick() runs at all - this is also the floor on
	// how promptly a queued check_run gets picked up, so it's deliberately
	// short (see cmd/server/main.go's SERVER_NODE_POLL_INTERVAL_MS, default
	// 1s). HeartbeatInterval below governs the much less time-sensitive
	// heartbeat write independently.
	Interval time.Duration
	// HeartbeatInterval throttles how often TouchHeartbeat actually writes,
	// separately from Interval - the node only needs to look "recently
	// seen" within NodeOnlineThreshold (cmd/server/main.go, default 90s),
	// so writing a heartbeat on every 1s tick was ~30-90x more DB writes
	// than that requires. Defaults to 30s (comfortably under the default
	// 90s threshold) when zero.
	HeartbeatInterval time.Duration
	MaxConcurrent     int
	// PollBatchLimit caps how many check_runs one tick claims at once.
	// Defaults to 50 when zero - mirrors agentapi.Handler.PollBatchLimit's
	// own default (cmd/server/main.go's POLL_BATCH_LIMIT), which the caller
	// is expected to pass in explicitly so the two dispatch paths (real
	// agents vs. this virtual node) share one config knob rather than one
	// of them silently staying hardcoded.
	PollBatchLimit int
	Log            *slog.Logger

	lastHeartbeat time.Time
}

// Run blocks, ticking until ctx is cancelled. Every tick also touches the
// node's heartbeat, so the virtual node reads as continuously online
// through the exact same Node.Online() threshold check real agents rely on
// - no special-casing needed anywhere else (node listing, dispatch, etc).
func (r *Runner) Run(ctx context.Context) {
	interval := r.Interval
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	r.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.tick(ctx)
		}
	}
}

func (r *Runner) tick(ctx context.Context) {
	heartbeatInterval := r.HeartbeatInterval
	if heartbeatInterval <= 0 {
		heartbeatInterval = 30 * time.Second
	}
	if time.Since(r.lastHeartbeat) >= heartbeatInterval {
		if err := r.Store.TouchHeartbeat(ctx, r.NodeID); err != nil {
			r.Log.Error("serveragent: touch heartbeat", "error", err)
		} else {
			r.lastHeartbeat = time.Now()
		}
	}

	pollBatchLimit := r.PollBatchLimit
	if pollBatchLimit <= 0 {
		pollBatchLimit = 50
	}
	jobs, err := r.Store.ClaimQueuedRuns(ctx, r.NodeID, pollBatchLimit)
	if err != nil {
		r.Log.Error("serveragent: claim queued runs", "error", err)
		return
	}
	if len(jobs) == 0 {
		return
	}

	maxConcurrent := r.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = 10
	}
	sem := make(chan struct{}, maxConcurrent)
	var wg sync.WaitGroup
	for _, job := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(job store.CheckRunJob) {
			defer wg.Done()
			defer func() { <-sem }()
			r.execute(ctx, job)
		}(job)
	}
	wg.Wait()
}

func (r *Runner) execute(ctx context.Context, job store.CheckRunJob) {
	checker, ok := checks.Get(string(job.Type))
	var res checks.Result
	if !ok {
		msg := "check type not supported by the server: " + string(job.Type)
		r.Log.Error("serveragent: unsupported check type", "type", job.Type, "check_run_id", job.CheckRunID)
		res = checks.Result{Success: false, ErrorMessage: &msg}
	} else {
		start := time.Now()
		// Zero-value NetConfig: the server's own default route, same as
		// /api/v1/server-ping already uses - there's no per-node network
		// interface to pin to here, this *is* the backend's own network path.
		res = checker.Run(ctx, checks.NetConfig{}, job.Target, job.Params)
		r.Log.Info("serveragent: check done", "type", job.Type, "target", job.Target,
			"success", res.Success, "elapsed_ms", time.Since(start).Milliseconds(), "check_run_id", job.CheckRunID)
	}

	raw := res.Raw
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if err := r.Store.CompleteCheckRun(ctx, job.CheckRunID, r.NodeID, res.Success, res.LatencyMs, res.StatusCode, res.ErrorMessage, raw); err != nil {
		r.Log.Error("serveragent: complete check_run", "check_run_id", job.CheckRunID, "error", err)
	}
}
