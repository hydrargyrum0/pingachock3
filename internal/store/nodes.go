package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/google/uuid"
)

const nodeColumns = `id, name, isp, city, country, agent_version, platform, last_heartbeat_at, secret_hash, blocked, is_virtual, tags, metadata, created_at`

func scanNode(row interface {
	Scan(dest ...any) error
}) (Node, error) {
	var n Node
	err := row.Scan(&n.ID, &n.Name, &n.ISP, &n.City, &n.Country, &n.AgentVersion, &n.Platform, &n.LastHeartbeatAt, &n.SecretHash, &n.Blocked, &n.IsVirtual, &n.Tags, &n.Metadata, &n.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Node{}, ErrNotFound
	}
	return n, err
}

func (s *Store) CreateNode(ctx context.Context, name, isp, city, secretHash string) (Node, error) {
	return scanNode(s.DB.QueryRowContext(ctx,
		`INSERT INTO nodes (name, isp, city, secret_hash)
		 VALUES ($1, $2, $3, $4)
		 RETURNING `+nodeColumns,
		name, isp, city, secretHash,
	))
}

func (s *Store) GetNode(ctx context.Context, id uuid.UUID) (Node, error) {
	return scanNode(s.DB.QueryRowContext(ctx, `SELECT `+nodeColumns+` FROM nodes WHERE id = $1`, id))
}

func (s *Store) GetNodeBySecretHash(ctx context.Context, secretHash string) (Node, error) {
	return scanNode(s.DB.QueryRowContext(ctx, `SELECT `+nodeColumns+` FROM nodes WHERE secret_hash = $1`, secretHash))
}

func (s *Store) ListNodes(ctx context.Context) ([]Node, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+nodeColumns+` FROM nodes ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// ListNodesByIDs preserves no particular order guarantee across ids; callers
// that need order should re-sort by n.ID themselves.
func (s *Store) ListNodesByIDs(ctx context.Context, ids []uuid.UUID) ([]Node, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	idStrs := make([]string, len(ids))
	for i, id := range ids {
		idStrs[i] = id.String()
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT `+nodeColumns+` FROM nodes WHERE id = ANY($1::uuid[])`, idStrs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// ListNodesByAnyTag returns nodes whose tags (jsonb array of strings)
// contain at least one of the given tags.
func (s *Store) ListNodesByAnyTag(ctx context.Context, tags []string) ([]Node, error) {
	if len(tags) == 0 {
		return nil, nil
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT `+nodeColumns+` FROM nodes WHERE tags ?| $1`, tags)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

func scanNodes(rows *sql.Rows) ([]Node, error) {
	var nodes []Node
	for rows.Next() {
		var n Node
		if err := rows.Scan(&n.ID, &n.Name, &n.ISP, &n.City, &n.Country, &n.AgentVersion, &n.Platform, &n.LastHeartbeatAt, &n.SecretHash, &n.Blocked, &n.IsVirtual, &n.Tags, &n.Metadata, &n.CreatedAt); err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

// GetVirtualNode returns the singleton virtual "server" node (see
// internal/serveragent), or ErrNotFound before it's been provisioned.
func (s *Store) GetVirtualNode(ctx context.Context) (Node, error) {
	return scanNode(s.DB.QueryRowContext(ctx, `SELECT `+nodeColumns+` FROM nodes WHERE is_virtual`))
}

// CreateVirtualNode provisions the singleton virtual "server" node. Callers
// should check GetVirtualNode first (see internal/serveragent.Ensure) -
// this doesn't check for you, though the idx_nodes_one_virtual unique index
// (migrations/0003) makes a second call fail loudly rather than silently
// creating a duplicate.
func (s *Store) CreateVirtualNode(ctx context.Context, name, secretHash string) (Node, error) {
	// country explicitly overrides the table's 'TM' default - the backend
	// isn't physically in Turkmenistan, and this node exists precisely to
	// measure reachability from wherever it *actually* runs (the Cloud Run
	// control plane), not to be mistaken for a Turkmen vantage point.
	return scanNode(s.DB.QueryRowContext(ctx,
		`INSERT INTO nodes (name, secret_hash, platform, country, is_virtual)
		 VALUES ($1, $2, 'server', '', true)
		 RETURNING `+nodeColumns,
		name, secretHash,
	))
}

func (s *Store) TouchHeartbeat(ctx context.Context, id uuid.UUID) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE nodes SET last_heartbeat_at = now() WHERE id = $1`, id)
	return err
}

func (s *Store) SetNodeAgentVersion(ctx context.Context, id uuid.UUID, version string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE nodes SET agent_version = $2 WHERE id = $1`, id, version)
	return err
}

func (s *Store) SetNodePlatform(ctx context.Context, id uuid.UUID, platform string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE nodes SET platform = $2 WHERE id = $1`, id, platform)
	return err
}

func (s *Store) SetNodeBlocked(ctx context.Context, id uuid.UUID, blocked bool) error {
	res, err := s.DB.ExecContext(ctx, `UPDATE nodes SET blocked = $2 WHERE id = $1`, id, blocked)
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
