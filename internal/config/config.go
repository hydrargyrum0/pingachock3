// Package config loads the node agent's config file - deliberately plain
// JSON (not YAML) to avoid a parsing dependency for a handful of fields
// operators set once at install time.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

type Config struct {
	NodeID     string `json:"node_id,omitempty"` // for operator reference/logging only; identity comes from NodeSecret
	NodeSecret string `json:"node_secret"`

	DirectURL string `json:"direct_url"`

	// Fronted transport (optional) - used as a fallback if Direct fails.
	// See internal/transport/fronted.go and docs/ARCHITECTURE.md.
	FrontDomain   string `json:"front_domain,omitempty"`
	FrontRealHost string `json:"front_real_host,omitempty"`

	// Which network interface to run checks through, and that interface's
	// own DNS servers (not the system-wide resolver, which can be silently
	// overridden by a VPN client) - set interactively via `configure`.
	// See internal/netiface.
	InterfaceName string   `json:"interface_name,omitempty"`
	LocalAddr     string   `json:"local_addr,omitempty"`
	DNSServers    []string `json:"dns_servers,omitempty"`

	PollIntervalSeconds int `json:"poll_interval_seconds"`
	MaxConcurrentChecks int `json:"max_concurrent_checks"`
}

const (
	DefaultPollIntervalSeconds = 30

	// DefaultMaxConcurrentChecks bounds how many checks internal/poller
	// runs at once (see poller.go's tick). Raised from the original 10 to
	// 50 - see internal/config's own
	// TestDefaultMaxConcurrentChecksKeepsRealisticBatchUnderBotDeadline
	// for the full arithmetic, but in short: a shelled-out `ping` against
	// an unreachable target can legitimately take up to
	// checks.DefaultPingWorstCase() (25s with default params), and this
	// app's whole point is finding targets exactly like that - a real
	// batch routinely contains several. At the old ceiling of 10, a batch
	// of e.g. 31 targets (a real user report, see internal/poller's
	// streamResults doc comment) needed 4 serialized "waves" to finish -
	// up to 4*25s=100s of execution alone, on top of up to
	// DefaultPollIntervalSeconds of latency just for the agent to notice
	// the batch exists, comfortably busting the bot's own
	// NODE_POLL_TIMEOUT_MS deadline (bot/src/pingachock-client.ts) even
	// after that deadline was itself already raised once (60s -> 100s,
	// see NODE_POLL_TIMEOUT_MS's own doc comment) for this same report. A
	// batch of 10 - "works" in that same report - fits the old ceiling in
	// one wave, which is exactly why it never reproduced there: this was
	// never really about batch size, it was about how many waves
	// MaxConcurrentChecks forces a batch that size into. 50 keeps a batch
	// up to 50 in one wave (well inside budget) and up to 100 in two
	// (still inside it); pinging is I/O-bound while it waits on a reply,
	// not CPU/memory heavy, so this many concurrent `ping` subprocesses is
	// cheap even on modest node hardware.
	DefaultMaxConcurrentChecks = 50

	// HistoricalMaxConcurrentChecksDefault is what DefaultMaxConcurrentChecks
	// used to be, before it was raised above. Load, below, treats a
	// persisted value that still equals this exactly like "unset" too -
	// not just a defensive gesture. This field has never been exposed via
	// any cmd/agent flag or interactive setup prompt (grep cmd/agent/main.go:
	// zero hits outside the two spots that default it), so every
	// already-deployed node's agent.json got this written automatically
	// the very first time `setup`/`configure` ever ran on it - never as a
	// deliberate operator choice. That matters because raising the
	// default here, on its own, would do *nothing* for those nodes:
	// `update` (cmd/agent/main.go's runUpdate, for shipping a new agent
	// binary to an already-configured node) explicitly never touches
	// agent.json, and re-running `configure`/`setup` preserves whatever
	// Read already finds on disk rather than re-deriving it - so a 10
	// already sitting in a real agent.json would otherwise survive every
	// future binary update forever, immune to this fix (and the next
	// person to raise this constant would hit the exact same trap).
	HistoricalMaxConcurrentChecksDefault = 10
)

// Read parses the config file if present, or returns a zero Config if it
// doesn't exist yet - used by `configure`, which fills in gaps
// interactively and doesn't require a pre-existing file.
func Read(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	return c, nil
}

// Load reads and validates the config, applying defaults - used by `run`.
func Load(path string) (Config, error) {
	c, err := Read(path)
	if err != nil {
		return Config{}, err
	}
	if c.NodeSecret == "" {
		return Config{}, errors.New("node_secret is required - run `configure` first")
	}
	if c.DirectURL == "" {
		return Config{}, errors.New("direct_url is required - run `configure` first")
	}
	if c.PollIntervalSeconds <= 0 {
		c.PollIntervalSeconds = DefaultPollIntervalSeconds
	}
	if c.MaxConcurrentChecks <= 0 || c.MaxConcurrentChecks == HistoricalMaxConcurrentChecksDefault {
		c.MaxConcurrentChecks = DefaultMaxConcurrentChecks
	}
	return c, nil
}

func Save(path string, c Config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}
