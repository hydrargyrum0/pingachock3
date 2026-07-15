// Package poller runs the node's main loop: poll the backend on an
// interval, execute whatever jobs come back concurrently, report results.
package poller

import (
	"context"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"pingachock/internal/agentstate"
	"pingachock/internal/checks"
	"pingachock/internal/transport"
)

type Poller struct {
	Transport     transport.Transport
	Interval      time.Duration
	AgentVersion  string
	MaxConcurrent int
	NetConfig     checks.NetConfig
	Log           *slog.Logger

	// StatePath, if set, is where a snapshot of "what is the agent doing"
	// gets written after every tick, so a separate invocation of this same
	// binary (the interactive menu) can show a live status without talking
	// to this running process directly. See internal/agentstate.
	StatePath string

	startedAt            time.Time
	consecutivePollFails int
}

// Run blocks, polling until ctx is cancelled. Starts after a random jitter
// (0..Interval) so a fleet of nodes doesn't all hit the backend in lockstep.
func (p *Poller) Run(ctx context.Context) {
	p.startedAt = time.Now()

	jitter := time.Duration(rand.Int63n(int64(p.Interval)))
	select {
	case <-ctx.Done():
		return
	case <-time.After(jitter):
	}

	p.tick(ctx)

	ticker := time.NewTicker(p.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.tick(ctx)
		}
	}
}

func (p *Poller) tick(ctx context.Context) {
	jobs, err := p.Transport.Poll(ctx, p.AgentVersion)
	if err != nil {
		p.consecutivePollFails++
		p.Log.Error("poll failed", "error", err, "consecutive_fails", p.consecutivePollFails, "transport", p.transportName())
		p.saveState(func(s *agentstate.State) {
			s.LastPollAt = time.Now()
			s.LastPollOK = false
			s.LastPollError = err.Error()
			s.ConsecutivePollFails = p.consecutivePollFails
		})
		return
	}

	p.consecutivePollFails = 0
	p.Log.Info("poll ok", "jobs", len(jobs), "transport", p.transportName())
	p.saveState(func(s *agentstate.State) {
		s.LastPollAt = time.Now()
		s.LastPollOK = true
		s.LastPollError = ""
		s.LastJobsCount = len(jobs)
		s.ConsecutivePollFails = 0
	})

	if len(jobs) == 0 {
		return
	}

	maxConcurrent := p.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = 10
	}
	sem := make(chan struct{}, maxConcurrent)
	var wg sync.WaitGroup
	var mu sync.Mutex
	results := make([]transport.ResultSubmission, 0, len(jobs))

	for _, job := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(job transport.Job) {
			defer wg.Done()
			defer func() { <-sem }()

			res := p.execute(ctx, job)
			mu.Lock()
			results = append(results, res)
			mu.Unlock()
		}(job)
	}
	wg.Wait()

	if err := p.Transport.PostResults(ctx, results); err != nil {
		p.Log.Error("post results failed", "error", err, "count", len(results))
		p.saveState(func(s *agentstate.State) {
			s.LastResultsAt = time.Now()
			s.LastResultsOK = false
			s.LastResultsError = err.Error()
		})
		return
	}

	p.Log.Info("results sent", "count", len(results))
	p.saveState(func(s *agentstate.State) {
		s.LastResultsAt = time.Now()
		s.LastResultsOK = true
		s.LastResultsError = ""
	})
}

func (p *Poller) execute(ctx context.Context, job transport.Job) transport.ResultSubmission {
	checker, ok := checks.Get(job.Type)
	if !ok {
		msg := "check type not supported by this agent: " + job.Type
		p.Log.Error("unsupported check type", "type", job.Type, "check_run_id", job.CheckRunID)
		return transport.ResultSubmission{CheckRunID: job.CheckRunID, Success: false, ErrorMessage: &msg}
	}

	start := time.Now()
	res := checker.Run(ctx, p.NetConfig, job.Target, job.Params)
	elapsed := time.Since(start)

	logArgs := []any{
		"type", job.Type, "target", job.Target, "success", res.Success,
		"elapsed_ms", elapsed.Milliseconds(), "check_run_id", job.CheckRunID,
	}
	if res.LatencyMs != nil {
		logArgs = append(logArgs, "latency_ms", *res.LatencyMs)
	}
	if res.ErrorMessage != nil {
		logArgs = append(logArgs, "check_error", *res.ErrorMessage)
	}
	p.Log.Info("check done", logArgs...)

	return transport.ResultSubmission{
		CheckRunID: job.CheckRunID, Success: res.Success, LatencyMs: res.LatencyMs,
		StatusCode: res.StatusCode, ErrorMessage: res.ErrorMessage, Raw: res.Raw,
	}
}

func (p *Poller) transportName() string {
	if n, ok := p.Transport.(interface{ Name() string }); ok {
		return n.Name()
	}
	return "unknown"
}

// saveState loads whatever's already on disk (so fields untouched by this
// tick, like the last successful results timestamp during a jobs-only
// tick, aren't clobbered back to zero), applies mutate, and writes it back.
func (p *Poller) saveState(mutate func(*agentstate.State)) {
	if p.StatePath == "" {
		return
	}
	s, _ := agentstate.Load(p.StatePath) // best-effort; missing/corrupt file just means zero value
	s.AgentVersion = p.AgentVersion
	s.StartedAt = p.startedAt
	s.Transport = p.transportName()
	mutate(&s)
	if err := agentstate.Save(p.StatePath, s); err != nil {
		p.Log.Warn("failed to save agent state", "error", err)
	}
}
