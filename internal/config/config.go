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
		c.PollIntervalSeconds = 30
	}
	if c.MaxConcurrentChecks <= 0 {
		c.MaxConcurrentChecks = 10
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
