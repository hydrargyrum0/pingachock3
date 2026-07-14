package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

func (s *Store) CreateCheckRuns(ctx context.Context, checkID uuid.UUID, nodeIDs []uuid.UUID) ([]CheckRun, error) {
	if len(nodeIDs) == 0 {
		return nil, nil
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	runs := make([]CheckRun, 0, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		var r CheckRun
		err := tx.QueryRowContext(ctx,
			`INSERT INTO check_runs (check_id, node_id) VALUES ($1, $2)
			 RETURNING id, check_id, node_id, status, dispatched_at, completed_at, created_at`,
			checkID, nodeID,
		).Scan(&r.ID, &r.CheckID, &r.NodeID, &r.Status, &r.DispatchedAt, &r.CompletedAt, &r.CreatedAt)
		if err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return runs, nil
}

// ClaimQueuedRuns atomically picks up to limit queued check_runs assigned to
// nodeID, marks them dispatched, and returns them ready to execute. Uses
// SKIP LOCKED so overlapping poll requests for the same node (shouldn't
// normally happen, but agents can retry) never double-dispatch a run.
func (s *Store) ClaimQueuedRuns(ctx context.Context, nodeID uuid.UUID, limit int) ([]CheckRunJob, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx,
		`SELECT cr.id, c.id, c.type, c.target, c.params
		 FROM check_runs cr
		 JOIN checks c ON c.id = cr.check_id
		 WHERE cr.node_id = $1 AND cr.status = 'queued'
		 ORDER BY cr.created_at
		 LIMIT $2
		 FOR UPDATE OF cr SKIP LOCKED`,
		nodeID, limit,
	)
	if err != nil {
		return nil, err
	}
	var jobs []CheckRunJob
	var idStrs []string
	for rows.Next() {
		var j CheckRunJob
		if err := rows.Scan(&j.CheckRunID, &j.CheckID, &j.Type, &j.Target, &j.Params); err != nil {
			rows.Close()
			return nil, err
		}
		jobs = append(jobs, j)
		idStrs = append(idStrs, j.CheckRunID.String())
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	if len(idStrs) > 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE check_runs SET status = 'dispatched', dispatched_at = now()
			 WHERE id = ANY($1::uuid[])`,
			idStrs,
		); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE checks SET status = 'running'
			 WHERE status = 'pending' AND id IN (
			   SELECT DISTINCT check_id FROM check_runs WHERE id = ANY($1::uuid[])
			 )`,
			idStrs,
		); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return jobs, nil
}

// CompleteCheckRun records the result of a run, marks it done/error, and
// re-derives the parent check's status once every run has finished.
// nodeID must match the run's assigned node - a node can only submit
// results for its own check_runs.
func (s *Store) CompleteCheckRun(ctx context.Context, checkRunID, nodeID uuid.UUID, success bool, latencyMs *int, statusCode, errorMessage *string, raw json.RawMessage) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	status := CheckRunStatusDone
	if !success {
		status = CheckRunStatusError
	}

	var checkID uuid.UUID
	err = tx.QueryRowContext(ctx,
		`UPDATE check_runs SET status = $3, completed_at = now()
		 WHERE id = $1 AND node_id = $2 RETURNING check_id`,
		checkRunID, nodeID, string(status),
	).Scan(&checkID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO results (check_run_id, success, latency_ms, status_code, error_message, raw)
		 VALUES ($1,$2,$3,$4,$5,$6)`,
		checkRunID, success, latencyMs, statusCode, errorMessage, raw,
	); err != nil {
		return err
	}

	if err := finalizeCheckStatusTx(ctx, tx, checkID); err != nil {
		return err
	}

	return tx.Commit()
}

// TimeoutStaleRuns marks queued/dispatched/running check_runs older than
// grace as timed out, and re-derives their parent checks' statuses. Meant to
// be called periodically by internal/sweeper.
func (s *Store) TimeoutStaleRuns(ctx context.Context, grace time.Duration) (int, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx,
		`UPDATE check_runs SET status = 'timeout', completed_at = now()
		 WHERE status IN ('queued','dispatched','running')
		   AND created_at < now() - ($1 * interval '1 second')
		 RETURNING check_id`,
		grace.Seconds(),
	)
	if err != nil {
		return 0, err
	}
	checkIDSet := map[uuid.UUID]struct{}{}
	count := 0
	for rows.Next() {
		var checkID uuid.UUID
		if err := rows.Scan(&checkID); err != nil {
			rows.Close()
			return 0, err
		}
		checkIDSet[checkID] = struct{}{}
		count++
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	rows.Close()

	for checkID := range checkIDSet {
		if err := finalizeCheckStatusTx(ctx, tx, checkID); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

// finalizeCheckStatusTx recomputes checks.status from its check_runs, once
// none remain queued/dispatched/running. Leaves 'cancelled' checks alone.
func finalizeCheckStatusTx(ctx context.Context, tx *sql.Tx, checkID uuid.UUID) error {
	var pending, done, errored int
	err := tx.QueryRowContext(ctx,
		`SELECT
		   count(*) FILTER (WHERE status IN ('queued','dispatched','running')),
		   count(*) FILTER (WHERE status = 'done'),
		   count(*) FILTER (WHERE status IN ('error','timeout'))
		 FROM check_runs WHERE check_id = $1`,
		checkID,
	).Scan(&pending, &done, &errored)
	if err != nil {
		return err
	}
	if pending > 0 {
		return nil
	}

	var status CheckStatus
	switch {
	case errored == 0:
		status = CheckStatusCompleted
	case done == 0:
		status = CheckStatusFailed
	default:
		status = CheckStatusPartial
	}

	_, err = tx.ExecContext(ctx,
		`UPDATE checks SET status = $2, completed_at = now() WHERE id = $1 AND status <> 'cancelled'`,
		checkID, string(status),
	)
	return err
}
