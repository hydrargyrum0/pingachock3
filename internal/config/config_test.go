package config

import (
	"path/filepath"
	"testing"
	"time"

	"pingachock/internal/checks"
)

func writeConfigFile(t *testing.T, c Config) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.json")
	if err := Save(path, c); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return path
}

// TestLoadSelfHealsHistoricalMaxConcurrentChecksDefault reproduces a real
// user report: pinging ~30 targets at once through a node came back with
// everything reported as failed, while the same targets in a batch of 10
// succeeded - even after installing a build that already contained the
// *previous* fix for this exact complaint (internal/poller's streamResults
// + NODE_POLL_TIMEOUT_MS 60s->100s, bot/src/pingachock-client.ts). Root
// cause this time: MaxConcurrentChecks' stock default of 10 forced a
// larger batch through several serialized "waves" (see
// DefaultMaxConcurrentChecks' own doc comment for the full arithmetic),
// each of which can legitimately take checks.DefaultPingWorstCase() - and
// a "new version installed, nothing changed" report is exactly what
// raising that default alone would produce for anyone already running: an
// already-configured node's agent.json has 10 written into it literally
// (config.Save marshals the whole struct, and MaxConcurrentChecks has no
// omitempty), `update` never touches agent.json at all, and re-running
// `configure` preserves whatever's already on disk - so only Load()
// itself, on every real agent start, can actually reach an
// already-deployed node.
func TestLoadSelfHealsHistoricalMaxConcurrentChecksDefault(t *testing.T) {
	path := writeConfigFile(t, Config{
		NodeSecret:          "secret",
		DirectURL:           "https://example.com",
		PollIntervalSeconds: 30,
		MaxConcurrentChecks: HistoricalMaxConcurrentChecksDefault, // exactly what every real deployed node's agent.json already has on disk
	})

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.MaxConcurrentChecks != DefaultMaxConcurrentChecks {
		t.Errorf("MaxConcurrentChecks = %d, want %d (Load must self-heal a persisted historical default, not just a bare code-level default bump)",
			got.MaxConcurrentChecks, DefaultMaxConcurrentChecks)
	}
}

// TestLoadLeavesAnyOtherMaxConcurrentChecksValueAlone proves the self-heal
// above is narrowly targeted at exactly the historical stock value, not a
// blanket "always override" - a genuinely different persisted number
// (whatever its origin) is left untouched.
func TestLoadLeavesAnyOtherMaxConcurrentChecksValueAlone(t *testing.T) {
	const customValue = 25
	path := writeConfigFile(t, Config{
		NodeSecret:          "secret",
		DirectURL:           "https://example.com",
		MaxConcurrentChecks: customValue,
	})

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.MaxConcurrentChecks != customValue {
		t.Errorf("MaxConcurrentChecks = %d, want %d (unchanged - only the exact historical default gets self-healed)", got.MaxConcurrentChecks, customValue)
	}
}

// TestLoadDefaultsAZeroMaxConcurrentChecksLikeAlways is the ordinary "never
// set at all" case (a hand-edited or very old config missing the field
// entirely) - unrelated to the self-heal above, kept to document that the
// original <=0 defaulting still works unchanged.
func TestLoadDefaultsAZeroMaxConcurrentChecksLikeAlways(t *testing.T) {
	path := writeConfigFile(t, Config{NodeSecret: "secret", DirectURL: "https://example.com"})

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.MaxConcurrentChecks != DefaultMaxConcurrentChecks {
		t.Errorf("MaxConcurrentChecks = %d, want %d", got.MaxConcurrentChecks, DefaultMaxConcurrentChecks)
	}
}

// botNodePollTimeout mirrors NODE_POLL_TIMEOUT_MS in
// bot/src/pingachock-client.ts - Go and TypeScript can't share a constant
// across the process boundary, so this is a deliberate, documented
// duplicate (same convention that file's own comments already use in the
// other direction, cross-referencing internal/poller and internal/checks
// by name). If that bot-side constant ever changes, this one needs to
// change with it or this test stops meaning anything.
const botNodePollTimeout = 100 * time.Second

// worstCaseBatchDuration is the same shape as bot/src/pingachock-client.ts'
// pollCheckUntilDone's deadline budget: up to one full poll interval for
// the agent to even notice a freshly-created batch exists, plus however
// many serialized "waves" MaxConcurrentChecks forces jobCount into, each
// taking up to checks.DefaultPingWorstCase() in the worst case (every wave
// happens to contain at least one unreachable target - not a contrived
// case, see DefaultMaxConcurrentChecks' doc comment).
func worstCaseBatchDuration(jobCount, maxConcurrent int) time.Duration {
	waves := (jobCount + maxConcurrent - 1) / maxConcurrent // ceil(jobCount/maxConcurrent)
	return time.Duration(DefaultPollIntervalSeconds)*time.Second + time.Duration(waves)*checks.DefaultPingWorstCase()
}

// TestDefaultMaxConcurrentChecksKeepsRealisticBatchUnderBotDeadline is the
// regression test for the bug TestLoadSelfHealsHistoricalMaxConcurrentChecksDefault
// documents above: it fails against the pre-fix DefaultMaxConcurrentChecks
// (10) for the exact batch size from the real user report (31 targets),
// and passes against the current one - so lowering this default again (or
// raising checks.DefaultPingWorstCase(), or lowering botNodePollTimeout)
// without re-checking this arithmetic gets caught here instead of by
// another "large batches all come back failed" report.
func TestDefaultMaxConcurrentChecksKeepsRealisticBatchUnderBotDeadline(t *testing.T) {
	// safetyMargin mirrors NODE_POLL_TIMEOUT_MS's own doc comment framing
	// (that constant itself keeps 20s of headroom under Telegraf's 120s
	// handlerTimeout) - this worst-case estimate should clear the bot's
	// deadline with room to spare, not just barely.
	const safetyMargin = 20 * time.Second

	for _, jobCount := range []int{10, 31, DefaultMaxConcurrentChecks, 100} {
		worst := worstCaseBatchDuration(jobCount, DefaultMaxConcurrentChecks)
		if worst > botNodePollTimeout-safetyMargin {
			t.Errorf("batch of %d targets: worst-case %v leaves less than %v of margin under the bot's %v deadline (DefaultMaxConcurrentChecks=%d too low for this batch size)",
				jobCount, worst, safetyMargin, botNodePollTimeout, DefaultMaxConcurrentChecks)
		}
	}
}
