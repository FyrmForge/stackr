package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// --- users (additions) ---

func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.GetContext(ctx, &n, `SELECT COUNT(*) FROM users`)
	return n, err
}

func (s *Store) ListUsers(ctx context.Context) ([]repo.User, error) {
	return list[repo.User](ctx, s, `SELECT * FROM users ORDER BY created_at`)
}

// --- ssh keys ---

// --- registries ---

func (s *Store) CreateRegistry(ctx context.Context, r *repo.Registry) error {
	_, err := s.db.NamedExecContext(ctx,
		`INSERT INTO registries (id, name, url, domain, username, password, managed, created_at)
		 VALUES (:id, :name, :url, :domain, :username, :password, :managed, :created_at)`, r)
	return err
}

func (s *Store) GetRegistry(ctx context.Context, id string) (*repo.Registry, error) {
	return get[repo.Registry](ctx, s, `SELECT * FROM registries WHERE id = ?`, id)
}

func (s *Store) GetManagedRegistry(ctx context.Context) (*repo.Registry, error) {
	return get[repo.Registry](ctx, s, `SELECT * FROM registries WHERE managed = 1`)
}

func (s *Store) ListRegistries(ctx context.Context) ([]repo.Registry, error) {
	return list[repo.Registry](ctx, s, `SELECT * FROM registries ORDER BY name`)
}

func (s *Store) UpdateRegistry(ctx context.Context, r *repo.Registry) error {
	_, err := s.db.NamedExecContext(ctx,
		`UPDATE registries SET name = :name, url = :url, domain = :domain, username = :username,
		 password = :password WHERE id = :id`, r)
	return err
}

func (s *Store) DeleteRegistry(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM registries WHERE id = ?`, id)
	return err
}

// --- api keys ---

func (s *Store) CreateAPIKey(ctx context.Context, k *repo.APIKey) error {
	_, err := s.db.NamedExecContext(ctx,
		`INSERT INTO api_keys (id, user_id, name, token_hash, scopes, created_at)
		 VALUES (:id, :user_id, :name, :token_hash, :scopes, :created_at)`, k)
	return err
}

func (s *Store) GetAPIKeyByHash(ctx context.Context, hash string) (*repo.APIKey, error) {
	return get[repo.APIKey](ctx, s, `SELECT * FROM api_keys WHERE token_hash = ?`, hash)
}

func (s *Store) ListAPIKeys(ctx context.Context) ([]repo.APIKey, error) {
	return list[repo.APIKey](ctx, s, `SELECT * FROM api_keys ORDER BY created_at`)
}

func (s *Store) DeleteAPIKey(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM api_keys WHERE id = ?`, id)
	return err
}

// --- connectors ---

func (s *Store) CreateConnector(ctx context.Context, cn *repo.Connector) error {
	_, err := s.db.NamedExecContext(ctx,
		`INSERT INTO connectors (id, org_id, provider, name, config, created_at)
		 VALUES (:id, :org_id, :provider, :name, :config, :created_at)`, cn)
	return err
}

func (s *Store) GetConnector(ctx context.Context, id string) (*repo.Connector, error) {
	return get[repo.Connector](ctx, s, `SELECT * FROM connectors WHERE id = ?`, id)
}

func (s *Store) ListConnectorsByOrg(ctx context.Context, orgID string) ([]repo.Connector, error) {
	return list[repo.Connector](ctx, s, `SELECT * FROM connectors WHERE org_id = ? ORDER BY name`, orgID)
}

func (s *Store) ListConnectors(ctx context.Context) ([]repo.Connector, error) {
	return list[repo.Connector](ctx, s, `SELECT * FROM connectors ORDER BY name`)
}

func (s *Store) UpdateConnector(ctx context.Context, cn *repo.Connector) error {
	_, err := s.db.NamedExecContext(ctx,
		`UPDATE connectors SET name = :name, config = :config WHERE id = :id`, cn)
	return err
}

func (s *Store) DeleteConnector(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM connectors WHERE id = ?`, id)
	return err
}

// --- node positions ---

func (s *Store) ListNodePositions(ctx context.Context, ownerID string) ([]repo.NodePosition, error) {
	return list[repo.NodePosition](ctx, s,
		`SELECT * FROM node_positions WHERE owner_id = ?`, ownerID)
}

func (s *Store) UpsertNodePosition(ctx context.Context, p *repo.NodePosition) error {
	_, err := s.db.NamedExecContext(ctx,
		`INSERT INTO node_positions (owner_id, node_id, x, y)
		 VALUES (:owner_id, :node_id, :x, :y)
		 ON CONFLICT (owner_id, node_id) DO UPDATE SET x = excluded.x, y = excluded.y`, p)
	return err
}

func (s *Store) DeleteNodePositions(ctx context.Context, ownerID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM node_positions WHERE owner_id = ?`, ownerID)
	return err
}

// --- annotations ---

func (s *Store) ListAnnotations(ctx context.Context, ownerID string) ([]repo.Annotation, error) {
	return list[repo.Annotation](ctx, s,
		`SELECT * FROM annotations WHERE owner_id = ? ORDER BY created_at`, ownerID)
}

// UpsertAnnotation validates then writes; the cap guards the same abuse the
// node-position limit does.
func (s *Store) UpsertAnnotation(ctx context.Context, a *repo.Annotation) error {
	if err := repo.ValidateAnnotation(a); err != nil {
		return err
	}
	// count only OTHER rows: at the cap an existing note must stay editable
	var n int
	if err := s.db.GetContext(ctx, &n,
		`SELECT COUNT(*) FROM annotations WHERE owner_id = ? AND id != ?`, a.OwnerID, a.ID); err != nil {
		return err
	}
	if n >= repo.MaxAnnotations {
		return fmt.Errorf("annotations: %d-note limit reached", repo.MaxAnnotations)
	}
	_, err := s.db.NamedExecContext(ctx,
		`INSERT INTO annotations (id, owner_id, kind, body, x, y, w, h, color, created_at, updated_at)
		 VALUES (:id, :owner_id, :kind, :body, :x, :y, :w, :h, :color, :created_at, :updated_at)
		 ON CONFLICT (id) DO UPDATE SET body = excluded.body, x = excluded.x, y = excluded.y,
		   w = excluded.w, h = excluded.h, color = excluded.color, updated_at = excluded.updated_at
		 WHERE annotations.owner_id = excluded.owner_id`, a)
	return err
}

func (s *Store) DeleteAnnotation(ctx context.Context, ownerID, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM annotations WHERE owner_id = ? AND id = ?`, ownerID, id)
	return err
}

// --- graph groups ---

func (s *Store) ListGraphGroups(ctx context.Context, ownerID string) ([]repo.GraphGroup, error) {
	return list[repo.GraphGroup](ctx, s,
		`SELECT * FROM graph_groups WHERE owner_id = ? ORDER BY created_at`, ownerID)
}

// UpsertGraphGroup validates then writes; the cap guards the same abuse the
// annotation limit does.
func (s *Store) UpsertGraphGroup(ctx context.Context, g *repo.GraphGroup) error {
	if err := repo.ValidateGraphGroup(g); err != nil {
		return err
	}
	// count only OTHER rows: at the cap an existing group must stay editable
	var n int
	if err := s.db.GetContext(ctx, &n,
		`SELECT COUNT(*) FROM graph_groups WHERE owner_id = ? AND id != ?`, g.OwnerID, g.ID); err != nil {
		return err
	}
	if n >= repo.MaxGraphGroups {
		return fmt.Errorf("graph groups: %d-group limit reached", repo.MaxGraphGroups)
	}
	_, err := s.db.NamedExecContext(ctx,
		`INSERT INTO graph_groups (id, owner_id, member_keys, created_at, updated_at)
		 VALUES (:id, :owner_id, :member_keys, :created_at, :updated_at)
		 ON CONFLICT (id) DO UPDATE SET member_keys = excluded.member_keys, updated_at = excluded.updated_at
		 WHERE graph_groups.owner_id = excluded.owner_id`, g)
	return err
}

func (s *Store) DeleteGraphGroup(ctx context.Context, ownerID, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM graph_groups WHERE owner_id = ? AND id = ?`, ownerID, id)
	return err
}

// --- metrics ---

func (s *Store) InsertMetric(ctx context.Context, m *repo.Metric) error {
	_, err := s.db.NamedExecContext(ctx,
		`INSERT INTO metrics (ref, ts, cpu_pct, mem_bytes, rx_bps, tx_bps)
		 VALUES (:ref, :ts, :cpu_pct, :mem_bytes, :rx_bps, :tx_bps)`, m)
	return err
}

func (s *Store) ListMetrics(ctx context.Context, ref string, since time.Time) ([]repo.Metric, error) {
	return list[repo.Metric](ctx, s,
		`SELECT * FROM metrics WHERE ref = ? AND ts >= ? ORDER BY ts`, ref, since.UTC())
}

func (s *Store) PruneMetrics(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM metrics WHERE ts < ?`, before.UTC())
	return err
}

// --- settings ---

func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.GetContext(ctx, &v, `SELECT value FROM settings WHERE key = ?`, key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// SaveNodePositions writes a whole layout atomically. Validation lives here
// rather than in each handler: three canvases post to this and the payload is
// untrusted, so the single choke point is where the rules belong.
func (s *Store) SaveNodePositions(ctx context.Context, ownerID string, ps []repo.NodePosition) error {
	if err := repo.ValidateNodePositions(ownerID, ps); err != nil {
		return err
	}
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed
	for i := range ps {
		row := ps[i] // copy: the caller still owns its slice
		row.OwnerID = ownerID
		if _, err := tx.NamedExecContext(ctx,
			`INSERT INTO node_positions (owner_id, node_id, x, y)
			 VALUES (:owner_id, :node_id, :x, :y)
			 ON CONFLICT (owner_id, node_id) DO UPDATE SET x = excluded.x, y = excluded.y`, row); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// --- org registry credentials ---

func (s *Store) CreateOrgRegistryCredential(ctx context.Context, c *repo.OrgRegistryCredential) error {
	_, err := s.db.NamedExecContext(ctx,
		`INSERT INTO org_registry_credentials (id, org_id, name, secret_hash, prefix, system, created_at)
		 VALUES (:id, :org_id, :name, :secret_hash, :prefix, :system, :created_at)`, c)
	return err
}

func (s *Store) GetOrgRegistryCredentialByHash(ctx context.Context, hash string) (*repo.OrgRegistryCredential, error) {
	return get[repo.OrgRegistryCredential](ctx, s,
		`SELECT * FROM org_registry_credentials WHERE secret_hash = ?`, hash)
}

func (s *Store) GetOrgRegistryCredential(ctx context.Context, id string) (*repo.OrgRegistryCredential, error) {
	return get[repo.OrgRegistryCredential](ctx, s,
		`SELECT * FROM org_registry_credentials WHERE id = ?`, id)
}

func (s *Store) ListOrgRegistryCredentials(ctx context.Context, orgID string) ([]repo.OrgRegistryCredential, error) {
	return list[repo.OrgRegistryCredential](ctx, s,
		`SELECT * FROM org_registry_credentials WHERE org_id = ? ORDER BY system DESC, created_at`, orgID)
}

func (s *Store) DeleteOrgRegistryCredential(ctx context.Context, id string) error {
	// The system credential is what stackr's own deploys push with; removing it
	// would break the org's next build with no way to tell from the UI why.
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM org_registry_credentials WHERE id = ? AND system = 0`, id)
	return err
}

func (s *Store) TouchOrgRegistryCredential(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE org_registry_credentials SET last_used_at = ? WHERE id = ?`, time.Now().UTC(), id)
	return err
}

// DeleteSystemOrgRegistryCredential removes a system row, which the public
// delete refuses. The only caller is the repair path: the registry password
// changed, so the stored hash can never match again and the row is replaced.
func (s *Store) DeleteSystemOrgRegistryCredential(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM org_registry_credentials WHERE id = ?`, id)
	return err
}
