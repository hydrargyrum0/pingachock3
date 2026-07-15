// Package agentstate persists a small snapshot of "what is the running
// agent process actually doing" to a JSON file, so a *separate* invocation
// of the same binary (the interactive menu) can show a live-ish summary
// without needing to talk to the running service process directly.
package agentstate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

type State struct {
	AgentVersion string    `json:"agent_version"`
	StartedAt    time.Time `json:"started_at"`
	Transport    string    `json:"transport"` // "direct" or "fronted"

	LastPollAt           time.Time `json:"last_poll_at,omitempty"`
	LastPollOK           bool      `json:"last_poll_ok"`
	LastPollError        string    `json:"last_poll_error,omitempty"`
	LastJobsCount        int       `json:"last_jobs_count"`
	ConsecutivePollFails int       `json:"consecutive_poll_fails"`

	LastResultsAt    time.Time `json:"last_results_at,omitempty"`
	LastResultsOK    bool      `json:"last_results_ok"`
	LastResultsError string    `json:"last_results_error,omitempty"`
}

func Path(dir string) string {
	return filepath.Join(dir, "agent.state.json")
}

// Save writes atomically (write to temp file + rename) so a menu process
// reading concurrently never sees a half-written file.
func Save(path string, s State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func Load(path string) (State, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return State{}, err
	}
	var s State
	err = json.Unmarshal(b, &s)
	return s, err
}
