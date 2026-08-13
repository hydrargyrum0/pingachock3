package poller

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"pingachock/internal/transport"
)

// fakeTransport is a minimal transport.Transport test double. Poll returns
// its canned jobs exactly once (so a test calling p.tick directly, not
// p.Run, never has to worry about a background ticker); PostResults
// records how many times and with what it was called, for assertions.
type fakeTransport struct {
	jobs        []transport.Job
	polled      bool
	postedCalls int
	lastPosted  []transport.ResultSubmission
}

func (f *fakeTransport) Poll(ctx context.Context, agentVersion string) ([]transport.Job, error) {
	if f.polled {
		return nil, nil
	}
	f.polled = true
	return f.jobs, nil
}

func (f *fakeTransport) PostResults(ctx context.Context, results []transport.ResultSubmission) error {
	f.postedCalls++
	f.lastPosted = results
	return nil
}

// testJob is a "tcp" check against a port nothing listens on (1 - the
// reserved tcpmux port), so it fails fast and deterministically without
// any real network dependency - same trick internal/checks/tls_test.go's
// TestTLSCheckerConnectionRefusedFailsAllAttempts already uses.
func testJob() transport.Job {
	return transport.Job{
		CheckRunID: uuid.New(),
		Type:       "tcp",
		Target:     "127.0.0.1",
		Params:     json.RawMessage(`{"port":1,"timeout_ms":500}`),
	}
}

func TestTickPostsResultsNormallyWhenPathNotSuspect(t *testing.T) {
	ft := &fakeTransport{jobs: []transport.Job{testJob()}}
	p := &Poller{Transport: ft, Log: slog.Default()}

	p.tick(context.Background())

	if ft.postedCalls != 1 {
		t.Fatalf("PostResults called %d times, want 1", ft.postedCalls)
	}
	if len(ft.lastPosted) != 1 {
		t.Fatalf("posted %d results, want 1", len(ft.lastPosted))
	}
}

func TestTickWithholdsResultsWhenPathSuspect(t *testing.T) {
	ft := &fakeTransport{jobs: []transport.Job{testJob()}}
	pathTest := &PathSelfTest{}
	pathTest.suspect = true // simulate an already-detected interference finding, without waiting on a real self-test tick
	p := &Poller{Transport: ft, PathTest: pathTest, Log: slog.Default()}

	p.tick(context.Background())

	if ft.postedCalls != 0 {
		t.Fatalf("PostResults called %d times, want 0 - path self-test currently suspects interference", ft.postedCalls)
	}
}

func TestTickWithNoPathTestConfiguredBehavesAsBefore(t *testing.T) {
	ft := &fakeTransport{jobs: []transport.Job{testJob()}}
	p := &Poller{Transport: ft, Log: slog.Default()} // PathTest left nil, same as every existing deployment before this feature

	p.tick(context.Background())

	if ft.postedCalls != 1 {
		t.Fatalf("PostResults called %d times, want 1 - a nil PathTest must never withhold anything", ft.postedCalls)
	}
}
