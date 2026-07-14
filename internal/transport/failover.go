package transport

import (
	"context"
	"log/slog"
	"sync"
)

// Failover tries primary (Direct) first; once it fails, permanently
// switches to fallback (Fronted) for the rest of the process's life. No
// flapping back - matches docs/ARCHITECTURE.md ("кэширует рабочий вариант").
type Failover struct {
	mu          sync.Mutex
	primary     Transport
	fallback    Transport
	useFallback bool
	log         *slog.Logger
}

func NewFailover(primary, fallback Transport, log *slog.Logger) *Failover {
	return &Failover{primary: primary, fallback: fallback, log: log}
}

func (f *Failover) current() Transport {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.useFallback && f.fallback != nil {
		return f.fallback
	}
	return f.primary
}

func (f *Failover) markFailed(used Transport) {
	if f.fallback == nil || used == f.fallback {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.useFallback {
		f.useFallback = true
		f.log.Warn("direct transport failed, switching to fronted transport")
	}
}

func (f *Failover) Poll(ctx context.Context, agentVersion string) ([]Job, error) {
	c := f.current()
	jobs, err := c.Poll(ctx, agentVersion)
	if err != nil {
		f.markFailed(c)
	}
	return jobs, err
}

func (f *Failover) PostResults(ctx context.Context, results []ResultSubmission) error {
	c := f.current()
	err := c.PostResults(ctx, results)
	if err != nil {
		f.markFailed(c)
	}
	return err
}
