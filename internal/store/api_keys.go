package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

func (s *Store) CreateAPIKey(ctx context.Context, accountID uuid.UUID, keyHash, label string, scopes []string) (APIKey, error) {
	var k APIKey
	err := s.DB.QueryRowContext(ctx,
		`INSERT INTO api_keys (account_id, key_hash, label, scopes)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id, account_id, key_hash, label, scopes, created_at, last_used_at, revoked_at`,
		accountID, keyHash, label, pq.Array(scopes),
	).Scan(&k.ID, &k.AccountID, &k.KeyHash, &k.Label, pq.Array(&k.Scopes), &k.CreatedAt, &k.LastUsedAt, &k.RevokedAt)
	return k, err
}

func (s *Store) GetAPIKeyByHash(ctx context.Context, keyHash string) (APIKey, error) {
	var k APIKey
	err := s.DB.QueryRowContext(ctx,
		`SELECT id, account_id, key_hash, label, scopes, created_at, last_used_at, revoked_at
		 FROM api_keys WHERE key_hash = $1 AND revoked_at IS NULL`,
		keyHash,
	).Scan(&k.ID, &k.AccountID, &k.KeyHash, &k.Label, pq.Array(&k.Scopes), &k.CreatedAt, &k.LastUsedAt, &k.RevokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return APIKey{}, ErrNotFound
	}
	return k, err
}

func (s *Store) TouchAPIKeyLastUsed(ctx context.Context, id uuid.UUID) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE api_keys SET last_used_at = now() WHERE id = $1`, id)
	return err
}
