package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

const checkColumns = `id, account_id, batch_id, type, target, params, node_selector, callback_url, status, warnings, created_at, completed_at`

type CreateCheckParams struct {
	AccountID    uuid.UUID
	BatchID      *uuid.UUID
	Type         CheckType
	Target       string
	Params       json.RawMessage
	NodeSelector json.RawMessage
	CallbackURL  *string
}

func scanCheck(row interface {
	Scan(dest ...any) error
}) (Check, error) {
	var c Check
	err := row.Scan(&c.ID, &c.AccountID, &c.BatchID, &c.Type, &c.Target, &c.Params, &c.NodeSelector, &c.CallbackURL, &c.Status, pq.Array(&c.Warnings), &c.CreatedAt, &c.CompletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Check{}, ErrNotFound
	}
	return c, err
}

func (s *Store) CreateCheck(ctx context.Context, p CreateCheckParams) (Check, error) {
	return scanCheck(s.DB.QueryRowContext(ctx,
		`INSERT INTO checks (account_id, batch_id, type, target, params, node_selector, callback_url)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)
		 RETURNING `+checkColumns,
		p.AccountID, p.BatchID, p.Type, p.Target, p.Params, p.NodeSelector, p.CallbackURL,
	))
}

func (s *Store) GetCheck(ctx context.Context, accountID, id uuid.UUID) (Check, error) {
	return scanCheck(s.DB.QueryRowContext(ctx,
		`SELECT `+checkColumns+` FROM checks WHERE id = $1 AND account_id = $2`, id, accountID,
	))
}

type ListChecksFilter struct {
	AccountID uuid.UUID
	BatchID   *uuid.UUID
	Status    *CheckStatus
	Limit     int
	Offset    int
}

func (s *Store) ListChecks(ctx context.Context, f ListChecksFilter) ([]Check, error) {
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	query := `SELECT ` + checkColumns + ` FROM checks WHERE account_id = $1`
	args := []any{f.AccountID}
	if f.BatchID != nil {
		args = append(args, *f.BatchID)
		query += fmt.Sprintf(" AND batch_id = $%d", len(args))
	}
	if f.Status != nil {
		args = append(args, string(*f.Status))
		query += fmt.Sprintf(" AND status = $%d", len(args))
	}
	args = append(args, limit)
	query += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d", len(args))
	args = append(args, f.Offset)
	query += fmt.Sprintf(" OFFSET $%d", len(args))

	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Check
	for rows.Next() {
		var c Check
		if err := rows.Scan(&c.ID, &c.AccountID, &c.BatchID, &c.Type, &c.Target, &c.Params, &c.NodeSelector, &c.CallbackURL, &c.Status, pq.Array(&c.Warnings), &c.CreatedAt, &c.CompletedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) SetCheckWarnings(ctx context.Context, id uuid.UUID, warnings []string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE checks SET warnings = $2 WHERE id = $1`, id, warnings)
	return err
}

func (s *Store) UpdateCheckStatus(ctx context.Context, id uuid.UUID, status CheckStatus) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE checks SET status = $2 WHERE id = $1`, id, string(status))
	return err
}

// CancelCheck cancels a check that's still pending/running and errors out any
// of its check_runs that haven't finished yet. No-op-safe: returns
// ErrNotFound if the check doesn't exist, doesn't belong to accountID, or is
// already in a terminal state.
func (s *Store) CancelCheck(ctx context.Context, accountID, id uuid.UUID) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`UPDATE checks SET status = 'cancelled', completed_at = now()
		 WHERE id = $1 AND account_id = $2 AND status IN ('pending','running')`,
		id, accountID,
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE check_runs SET status = 'error', completed_at = now()
		 WHERE check_id = $1 AND status IN ('queued','dispatched','running')`,
		id,
	); err != nil {
		return err
	}

	return tx.Commit()
}
