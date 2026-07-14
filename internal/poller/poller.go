// Package poller runs the node's main loop: poll the backend on an
// interval, execute whatever jobs come back concurrently, report results.
package poller

import (
	"context"
	"log/slog"
	"math/rand"
	"sync"
	"time"

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
}

// Run blocks, polling until ctx is cancelled. Starts after a random jitter
// (0..Interval) so a fleet of nodes doesn't all hit the backend in lockstep.
func (p *Poller) Run(ctx context.Context) {
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
		p.Log.Error("poll failed", "error", err)
		return
	}
	if len(jobs) == 0 {
		return
	}
	p.Log.Info("received jobs", "count", len(jobs))

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
		p.Log.Error("post results failed", "error", err)
	}
}

func (p *Poller) execute(ctx context.Context, job transport.Job) transport.ResultSubmission {
	checker, ok := checks.Get(job.Type)
	if !ok {
		msg := "check type not supported by this agent: " + job.Type
		return transport.ResultSubmission{CheckRunID: job.CheckRunID, Success: false, ErrorMessage: &msg}
	}

	res := checker.Run(ctx, p.NetConfig, job.Target, job.Params)
	p.Log.Info("check done", "type", job.Type, "target", job.Target, "success", res.Success)

	return transport.ResultSubmission{
		CheckRunID: job.CheckRunID, Success: res.Success, LatencyMs: res.LatencyMs,
		StatusCode: res.StatusCode, ErrorMessage: res.ErrorMessage, Raw: res.Raw,
	}
}
