// Package dispatch resolves a check's node_selector into concrete node IDs
// to run against. See docs/ARCHITECTURE.md ("Поток диспетчеризации").
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"pingachock/internal/store"
)

var ErrEmptySelector = errors.New("node_selector must specify one of: node_ids, tags, all")

type NodeSelector struct {
	NodeIDs        []uuid.UUID `json:"node_ids,omitempty"`
	Tags           []string    `json:"tags,omitempty"`
	All            bool        `json:"all,omitempty"`
	IncludeOffline bool        `json:"include_offline,omitempty"`
}

// Resolve turns a node_selector into the concrete list of node IDs to
// dispatch check_runs to, plus any warnings worth surfacing to the caller.
//
// node_ids is an explicit choice: every named node gets a check_run even if
// currently offline (it'll run once the node reconnects), and offline nodes
// are called out in the warnings. tags/all are "run on what's available
// now": offline nodes are silently excluded unless IncludeOffline is set.
func Resolve(ctx context.Context, s *store.Store, sel NodeSelector, onlineThreshold time.Duration) ([]uuid.UUID, []string, error) {
	switch {
	case len(sel.NodeIDs) > 0:
		nodes, err := s.ListNodesByIDs(ctx, sel.NodeIDs)
		if err != nil {
			return nil, nil, fmt.Errorf("list nodes by id: %w", err)
		}
		found := make(map[uuid.UUID]store.Node, len(nodes))
		for _, n := range nodes {
			found[n.ID] = n
		}

		var warnings []string
		ids := make([]uuid.UUID, 0, len(sel.NodeIDs))
		for _, id := range sel.NodeIDs {
			n, ok := found[id]
			if !ok {
				warnings = append(warnings, fmt.Sprintf("node %s not found", id))
				continue
			}
			ids = append(ids, id)
			if !n.Online(onlineThreshold) {
				warnings = append(warnings, fmt.Sprintf("node %s (%s) is currently offline, run will wait for it to reconnect", n.ID, n.Name))
			}
		}
		return ids, warnings, nil

	case sel.All:
		nodes, err := s.ListNodes(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("list nodes: %w", err)
		}
		return filterOnline(nodes, sel.IncludeOffline, onlineThreshold), nil, nil

	case len(sel.Tags) > 0:
		nodes, err := s.ListNodesByAnyTag(ctx, sel.Tags)
		if err != nil {
			return nil, nil, fmt.Errorf("list nodes by tag: %w", err)
		}
		return filterOnline(nodes, sel.IncludeOffline, onlineThreshold), nil, nil

	default:
		return nil, nil, ErrEmptySelector
	}
}

func filterOnline(nodes []store.Node, includeOffline bool, threshold time.Duration) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(nodes))
	for _, n := range nodes {
		if includeOffline || n.Online(threshold) {
			ids = append(ids, n.ID)
		}
	}
	return ids
}
