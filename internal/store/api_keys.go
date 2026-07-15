package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

func (s *Store) CreateAPIKey(ctx context.Context, accountID uuid.UUID, keyHash, label string, scopes []string) (APIKey, error) {
	if scopes == nil {
		// pq.Array(nil) encodes as SQL NULL, which the NOT NULL scopes
		// column rejects - a nil slice means "no scopes given", not "store
		// nothing", so normalize to empty here rather than pushing this
		// distinction onto every caller.
		scopes = []string{}
	}
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

func (s *Store) ListAPIKeysByAccount(ctx context.Context, accountID uuid.UUID) ([]APIKey, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, account_id, key_hash, label, scopes, created_at, last_used_at, revoked_at
		 FROM api_keys WHERE account_id = $1 ORDER BY created_at`,
		accountID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []APIKey
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.AccountID, &k.KeyHash, &k.Label, pq.Array(&k.Scopes), &k.CreatedAt, &k.LastUsedAt, &k.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeAPIKey soft-revokes a key (revoked_at set) rather than deleting it,
// so it stays visible in ListAPIKeysByAccount for audit purposes. Returns
// ErrNotFound if the key doesn't exist or is already revoked.
func (s *Store) RevokeAPIKey(ctx context.Context, id uuid.UUID) error {
	res, err := s.DB.ExecContext(ctx,
		`UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, id,
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
	return nil
}
