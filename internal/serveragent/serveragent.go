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

	"pingachock/internal/auth"
	"pingachock/internal/checks"
	"pingachock/internal/store"
)

// NodeName is the fixed name the virtual node is provisioned under - what
// shows up as this "node"'s name in GET /api/v1/nodes.
const NodeName = "server"

// Ensure idempotently provisions the singleton virtual "server" node - safe
// to call on every boot. Its secret is generated but never actually used
// for auth: unlike a real agent, the virtual node is never reached over
// HTTP (see Runner), it's just a placeholder to satisfy nodes.secret_hash's
// NOT NULL constraint.
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
	return st.CreateVirtualNode(ctx, NodeName, auth.HashToken(secret))
}

// Runner polls its own store for check_runs assigned to the virtual server
// node and executes them in-process - no HTTP hop between "poll" and
// "execute" the way a remote agent has, so the interval can be short (this
// is what stands in for the old /api/v1/server-ping's synchronous feel).
type Runner struct {
	Store         *store.Store
	NodeID        uuid.UUID
	Interval      time.Duration
	MaxConcurrent int
	Log           *slog.Logger
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
	if err := r.Store.TouchHeartbeat(ctx, r.NodeID); err != nil {
		r.Log.Error("serveragent: touch heartbeat", "error", err)
	}

	jobs, err := r.Store.ClaimQueuedRuns(ctx, r.NodeID, 50)
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
