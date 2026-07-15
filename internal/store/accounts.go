package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/google/uuid"
)

func (s *Store) CreateAccount(ctx context.Context, name string) (Account, error) {
	var a Account
	err := s.DB.QueryRowContext(ctx,
		`INSERT INTO accounts (name) VALUES ($1) RETURNING id, name, created_at`,
		name,
	).Scan(&a.ID, &a.Name, &a.CreatedAt)
	return a, err
}

func (s *Store) GetAccount(ctx context.Context, id uuid.UUID) (Account, error) {
	var a Account
	err := s.DB.QueryRowContext(ctx,
		`SELECT id, name, created_at FROM accounts WHERE id = $1`, id,
	).Scan(&a.ID, &a.Name, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	return a, err
}

func (s *Store) ListAccounts(ctx context.Context) ([]Account, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, name, created_at FROM accounts ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Account
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.ID, &a.Name, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
