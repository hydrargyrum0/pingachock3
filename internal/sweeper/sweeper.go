// Package sweeper periodically times out check_runs whose target node never
// picked them up (or died mid-run), so a check can't hang in 'pending'/
// 'running' forever. See docs/ARCHITECTURE.md.
package sweeper

import (
	"context"
	"log/slog"
	"time"

	"pingachock/internal/store"
)

type Sweeper struct {
	store    *store.Store
	interval time.Duration
	grace    time.Duration
	log      *slog.Logger
}

func New(s *store.Store, interval, grace time.Duration, log *slog.Logger) *Sweeper {
	return &Sweeper{store: s, interval: interval, grace: grace, log: log}
}

// Run blocks, sweeping every interval until ctx is cancelled.
func (sw *Sweeper) Run(ctx context.Context) {
	ticker := time.NewTicker(sw.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := sw.store.TimeoutStaleRuns(ctx, sw.grace)
			if err != nil {
				sw.log.Error("sweep failed", "error", err)
				continue
			}
			if n > 0 {
				sw.log.Info("timed out stale check_runs", "count", n)
			}
		}
	}
}
