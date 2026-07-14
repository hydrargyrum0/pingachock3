package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

func (s *Store) ListRunsForCheck(ctx context.Context, checkID uuid.UUID) ([]RunWithResult, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT cr.id, cr.check_id, cr.node_id, cr.status, cr.dispatched_at, cr.completed_at, cr.created_at,
		        n.id, n.name, n.isp, n.city, n.country,
		        r.id, r.success, r.latency_ms, r.status_code, r.error_message, r.raw, r.created_at
		 FROM check_runs cr
		 JOIN nodes n ON n.id = cr.node_id
		 LEFT JOIN results r ON r.check_run_id = cr.id
		 WHERE cr.check_id = $1
		 ORDER BY cr.created_at`,
		checkID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RunWithResult
	for rows.Next() {
		var rw RunWithResult
		var resID *uuid.UUID
		var success *bool
		var latency *int
		var statusCode, errMsg *string
		var raw json.RawMessage
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
				Raw:          raw,
				CreatedAt:    *resCreatedAt,
			}
		}
		out = append(out, rw)
	}
	return out, rows.Err()
}
