package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// ListRunsForCheck is now a thin wrapper around ListRunsForChecks - kept
// for GetCheck's single-check ?expand=runs path, which has no need for the
// plural version's grouping. See ListRunsForChecks' own doc comment for
// why the plural form exists at all.
func (s *Store) ListRunsForCheck(ctx context.Context, checkID uuid.UUID) ([]RunWithResult, error) {
	byCheck, err := s.ListRunsForChecks(ctx, []uuid.UUID{checkID})
	if err != nil {
		return nil, err
	}
	return byCheck[checkID], nil
}

// ListRunsForChecks is ListRunsForCheck generalized to many check IDs at
// once, grouped by check_id in the returned map - one query for a whole
// page of checks instead of one query per check (an N+1 a naive
// "expand=runs on a list endpoint" would otherwise turn into). Exists
// specifically for GET /api/v1/checks?batch_id=...&expand=runs
// (checks.go's ListChecks), which needs full run/result detail for up to
// a whole page (ListChecksFilter's own 200-row cap) of checks in one
// response - the earlier per-check-ID polling this replaces
// (bot/src/pingachock-client.ts's old pollCheckUntilDone loop) made a
// large batch's own status/result fetching the very bottleneck it was
// trying to report on. A missing key in the returned map (rather than an
// empty slice) is impossible by construction - every input ID either has
// rows or doesn't, and Go's zero-value nil slice for "no rows" behaves
// identically to an explicit empty one at every call site.
func (s *Store) ListRunsForChecks(ctx context.Context, checkIDs []uuid.UUID) (map[uuid.UUID][]RunWithResult, error) {
	out := make(map[uuid.UUID][]RunWithResult, len(checkIDs))
	if len(checkIDs) == 0 {
		return out, nil
	}

	rows, err := s.DB.QueryContext(ctx,
		`SELECT cr.id, cr.check_id, cr.node_id, cr.status, cr.dispatched_at, cr.completed_at, cr.created_at,
		        n.id, n.name, n.isp, n.city, n.country,
		        r.id, r.success, r.latency_ms, r.status_code, r.error_message, r.raw, r.created_at
		 FROM check_runs cr
		 JOIN nodes n ON n.id = cr.node_id
		 LEFT JOIN results r ON r.check_run_id = cr.id
		 WHERE cr.check_id = ANY($1)
		 ORDER BY cr.check_id, cr.created_at`,
		pq.Array(checkIDs),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var rw RunWithResult
		var resID *uuid.UUID
		var success *bool
		var latency *int
		var statusCode, errMsg *string
		var raw []byte
		var resCreatedAt *time.Time

		if err := rows.Scan(
			&rw.Run.ID, &rw.Run.CheckID, &rw.Run.NodeID, &rw.Run.Status, &rw.Run.DispatchedAt, &rw.Run.CompletedAt, &rw.Run.CreatedAt,
			&rw.Node.ID, &rw.Node.Name, &rw.Node.ISP, &rw.Node.City, &rw.Node.Country,
			&resID, &success, &latency, &statusCode, &errMsg, &raw, &resCreatedAt,
		); err != nil {
			return nil, err
		}

		if resID != nil {
			rw.Result = &Result{
				ID:           *resID,
				CheckRunID:   rw.Run.ID,
				Success:      *success,
				LatencyMs:    latency,
				StatusCode:   statusCode,
				ErrorMessage: errMsg,
				Raw:          json.RawMessage(raw),
				CreatedAt:    *resCreatedAt,
			}
		}
		out[rw.Run.CheckID] = append(out[rw.Run.CheckID], rw)
	}
	return out, rows.Err()
}
