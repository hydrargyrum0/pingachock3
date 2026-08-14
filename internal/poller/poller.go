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

	// PathTest, when set, is consulted right before every result
	// submission - while it reports Suspect(), this tick's results are
	// withheld instead of posted (see tick, below). nil (the default,
	// unless cmd/agent wires one up) means "never withhold anything",
	// identical to this feature not existing at all.
	PathTest *PathSelfTest

	Log *slog.Logger

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
	resultsCh := make(chan transport.ResultSubmission, len(jobs))

	for _, job := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(job transport.Job) {
			defer wg.Done()
			defer func() { <-sem }()
			resultsCh <- p.execute(ctx, job)
		}(job)
	}
	go func() {
		wg.Wait()
		close(resultsCh)
	}()

	p.streamResults(ctx, resultsCh)
}

// resultFlushInterval/resultFlushBatchSize bound how long a finished
// result can sit waiting before streamResults reports it - see
// streamResults' own doc comment for why this exists at all. Neither
// value is tuned for a specific batch size; both just need to stay small
// relative to how long a real check can legitimately take (seconds), so
// even a large batch's fastest results are reported promptly rather than
// in lockstep with its slowest one.
const (
	resultFlushInterval  = 500 * time.Millisecond
	resultFlushBatchSize = 10
)

// streamResults drains resultsCh (closed once every worker goroutine for
// this tick has finished executing its job) and posts whatever has
// accumulated periodically, instead of collecting every result and
// posting them all in one call only after the very last one finishes.
//
// That "one call at the very end" design was the actual bug behind a real
// report: pinging a large batch of addresses at once came back with every
// single one reported as failed, while the same addresses in smaller
// batches succeeded. A big batch of check targets routinely includes some
// that are slow or genuinely unreachable - that's the whole point of a
// censorship-monitoring tool, not an edge case - and MaxConcurrent
// throttles how many run at once, so a big batch takes several times
// longer overall than a small one even before factoring in stragglers.
// Every other result in that same tick, however fast it personally
// finished, used to sit done-and-ready but unsent until the single
// slowest job in the whole tick finally finished too - easily blowing
// past the bot's own fixed per-check poll timeout
// (NODE_POLL_TIMEOUT_MS in bot/src/pingachock-client.ts), which then gave
// up and reported those already-succeeded checks as failed simply because
// it stopped waiting before the agent got around to telling it otherwise.
//
// Flushing periodically instead decouples "how long until this result is
// reported" from "how many other jobs happen to share this tick and how
// slow the slowest of them is" - a result is now reported within
// resultFlushInterval of finishing, full stop, regardless of batch size.
func (p *Poller) streamResults(ctx context.Context, resultsCh <-chan transport.ResultSubmission) {
	ticker := time.NewTicker(resultFlushInterval)
	defer ticker.Stop()

	var pending []transport.ResultSubmission
	flush := func() {
		if len(pending) == 0 {
			return
		}
		batch := pending
		pending = nil
		p.postBatch(ctx, batch)
	}

	for {
		select {
		case res, ok := <-resultsCh:
			if !ok {
				flush()
				return
			}
			pending = append(pending, res)
			if len(pending) >= resultFlushBatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// postBatch is tick's old end-of-run submission logic, unchanged in
// substance (same withhold check, same success/failure logging and
// state-saving) - just now called once per flush instead of once per
// tick, so a busy tick logs one "results sent" line per batch actually
// sent rather than a single line for the whole tick. That's a more
// accurate reflection of what happened, not a loss of information: for
// the common case of a small tick that fits in one flush, the log output
// is identical to before.
func (p *Poller) postBatch(ctx context.Context, results []transport.ResultSubmission) {
	if p.PathTest != nil && p.PathTest.Suspect() {
		p.Log.Warn("withholding a batch of results - path self-test currently suspects VPN/proxy interception", "count", len(results))
		return
	}

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
