package poller

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"pingachock/internal/checks"
	"pingachock/internal/transport"
)

// fakeTransport is a minimal transport.Transport test double. Poll returns
// its canned jobs exactly once (so a test calling p.tick directly, not
// p.Run, never has to worry about a background ticker); PostResults
// records how many times, when, and with what it was called, for
// assertions - protected by a mutex since tick's own PostResults call
// itself isn't necessarily the only concurrent thing touching this fake
// once tick starts posting results incrementally instead of once at the
// end.
type fakeTransport struct {
	jobs   []transport.Job
	polled bool

	mu          sync.Mutex
	postedCalls int
	lastPosted  []transport.ResultSubmission
	postedAt    []time.Time
}

func (f *fakeTransport) Poll(ctx context.Context, agentVersion string) ([]transport.Job, error) {
	if f.polled {
		return nil, nil
	}
	f.polled = true
	return f.jobs, nil
}

func (f *fakeTransport) PostResults(ctx context.Context, results []transport.ResultSubmission) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.postedCalls++
	f.lastPosted = results
	f.postedAt = append(f.postedAt, time.Now())
	return nil
}

func (f *fakeTransport) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.postedCalls
}

func (f *fakeTransport) firstPostedAt() (time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.postedAt) == 0 {
		return time.Time{}, false
	}
	return f.postedAt[0], true
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

// slowResolvingJob targets a hostname (not a literal IP) resolved through
// a deliberately slow test-only resolver (see the test below) - a
// deterministic, environment-independent stand-in for the realistic case
// this app hits constantly: some fraction of any real target batch is
// slow or unreachable (that's the entire point of a censorship-monitoring
// tool). A real black-holed IP was tried first and rejected: how long an
// unreachable address actually takes to fail depends on the real network
// path (this dev machine's own VPN answered one immediately instead of
// timing out), which is exactly the kind of environment-dependent flakiness
// a regression test can't rely on.
func slowResolvingJob() transport.Job {
	return transport.Job{
		CheckRunID: uuid.New(),
		Type:       "tcp",
		Target:     "slow-lookup.pingachock-test.invalid",
		Params:     json.RawMessage(`{"port":80,"timeout_ms":800}`),
	}
}

// TestTickPostsFastResultsWithoutWaitingForASlowStraggler reproduces a
// real user report: pinging a large batch of addresses at once came back
// with every single one reported as failed, while the exact same
// addresses in smaller batches succeeded. Root cause: a batch containing
// even one slow/unreachable target (routine for this app's real targets -
// slow and unreachable targets are exactly what it exists to find) used
// to delay reporting of every OTHER target's result in the same tick,
// however fast they'd already finished - results only ever got posted
// once, in a single PostResults call, after every job in the tick had
// finished. A big batch is far more likely to contain at least one slow
// target than a small one, so this made big batches systematically less
// likely to report back within the bot's fixed poll timeout
// (NODE_POLL_TIMEOUT_MS in bot/src/pingachock-client.ts) than small ones -
// their fast results were sitting done and ready, just never sent.
func TestTickPostsFastResultsWithoutWaitingForASlowStraggler(t *testing.T) {
	// The 3 fast jobs below target a literal IP (127.0.0.1), which
	// resolveIP short-circuits before ever touching the resolver - so this
	// slow resolver only ever affects slowResolvingJob's hostname target,
	// not them.
	slowResolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			// select on ctx.Done(), not a bare time.Sleep: the stdlib
			// resolver can invoke Dial more than once per lookup (e.g. a
			// UDP attempt then a TCP fallback), and an unconditional sleep
			// would let each of those add its own full delay on top of the
			// last regardless of the lookup's own deadline already having
			// passed - a real dial would get cut short by ctx instead.
			select {
			case <-time.After(400 * time.Millisecond):
			case <-ctx.Done():
			}
			return nil, errors.New("simulated slow lookup")
		},
	}

	ft := &fakeTransport{jobs: []transport.Job{testJob(), testJob(), testJob(), slowResolvingJob()}}
	p := &Poller{
		Transport:     ft,
		MaxConcurrent: 10,
		NetConfig:     checks.NetConfig{Resolver: slowResolver},
		Log:           slog.Default(),
	}

	start := time.Now()
	p.tick(context.Background())

	postedAt, ok := ft.firstPostedAt()
	if !ok {
		t.Fatal("PostResults was never called")
	}
	firstPostDelay := postedAt.Sub(start)
	if firstPostDelay > time.Second {
		t.Errorf("first PostResults call happened %v after tick start, want under 1s - the 3 fast jobs finish almost instantly and shouldn't have to wait for the slow straggler (~1.6s+)", firstPostDelay)
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
