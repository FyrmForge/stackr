package sqlite

import (
	"context"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

func (s *Store) GetServer(ctx context.Context, id string) (*repo.Server, error) {
	return get[repo.Server](ctx, s, `SELECT * FROM servers WHERE id = ?`, id)
}

// GetServerByNodeID finds the row mirroring one swarm node. Used to reconcile
// `docker node ls` against the table on every servers-list load.
func (s *Store) GetServerByNodeID(ctx context.Context, nodeID string) (*repo.Server, error) {
	return get[repo.Server](ctx, s, `SELECT * FROM servers WHERE node_id = ?`, nodeID)
}

func (s *Store) ListServers(ctx context.Context) ([]repo.Server, error) {
	return list[repo.Server](ctx, s, `SELECT * FROM servers ORDER BY name`)
}

func (s *Store) CreateServer(ctx context.Context, sv *repo.Server) error {
	_, err := s.db.NamedExecContext(ctx, `INSERT INTO servers
		(id, name, kind, endpoint, settings, created_at, node_id, hostname, address, role, status, last_seen_at)
		VALUES (:id, :name, :kind, :endpoint, :settings, :created_at, :node_id, :hostname, :address, :role, :status, :last_seen_at)`, sv)
	return err
}

func (s *Store) UpdateServer(ctx context.Context, sv *repo.Server) error {
	_, err := s.db.NamedExecContext(ctx,
		`UPDATE servers SET name = :name, endpoint = :endpoint, settings = :settings,
		 node_id = :node_id, hostname = :hostname, address = :address,
		 role = :role, status = :status, last_seen_at = :last_seen_at WHERE id = :id`, sv)
	return err
}

func (s *Store) DeleteServer(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM servers WHERE id = ?`, id)
	return err
}

// --- join keys ------------------------------------------------------------

func (s *Store) CreateJoinKey(ctx context.Context, k *repo.JoinKey) error {
	_, err := s.db.NamedExecContext(ctx, `INSERT INTO join_keys
		(key, server_id, address, expires_at, used_at, created_at)
		VALUES (:key, :server_id, :address, :expires_at, :used_at, :created_at)`, k)
	return err
}

func (s *Store) GetJoinKey(ctx context.Context, key string) (*repo.JoinKey, error) {
	return get[repo.JoinKey](ctx, s, `SELECT * FROM join_keys WHERE key = ?`, key)
}

// LatestJoinKey is the newest key issued for a server, so a pending row can
// show its script again without minting a second one.
func (s *Store) LatestJoinKey(ctx context.Context, serverID string) (*repo.JoinKey, error) {
	return get[repo.JoinKey](ctx, s,
		`SELECT * FROM join_keys WHERE server_id = ? ORDER BY created_at DESC LIMIT 1`, serverID)
}

// BurnServerJoinKeys spends every key a server still has unspent, and returns
// how many it took.
//
// This is what revoking means here. IssueKey only ever inserts, so before this
// existed a second "new script" left the first key working for the rest of its
// hour: an operator who pasted a join command somewhere they should not have
// had nothing in the product that would stop it.
func (s *Store) BurnServerJoinKeys(ctx context.Context, serverID string) (int, error) {
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`UPDATE join_keys SET used_at = ? WHERE server_id = ? AND used_at IS NULL AND expires_at > ?`,
		now, serverID, now)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// BurnJoinKey marks a key used. One-time by construction: the update only
// matches a key nobody has spent yet, so two joins racing the same key means
// exactly one of them gets a row back.
func (s *Store) BurnJoinKey(ctx context.Context, key string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE join_keys SET used_at = ? WHERE key = ? AND used_at IS NULL AND expires_at > ?`,
		time.Now().UTC(), key, time.Now().UTC())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
