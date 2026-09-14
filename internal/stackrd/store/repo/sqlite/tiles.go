package sqlite

import (
	"context"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/config/envutil"
	"github.com/FyrmForge/stackr/internal/stackrd/config/secrets"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

const tileCols = `id, stack_id, environment_id, name, slug, kind, source_type, git_url, git_branch, connector_id,
	image_ref, dockerfile_path, build_context, env, build_args, volumes,
	container_port, healthcheck_cmd, webhook_token, status, cpu_limit, mem_limit_mb,
	basic_auth_user, basic_auth_hash, sec_headers, published_ports, traefik_override, watch_paths,
	update_policy, image_digest, latest_digest, wait_for_ci,
	user, shm_size_mb, privileged, devices, restart_policy,
	healthcheck_interval_s, healthcheck_timeout_s, healthcheck_retries, healthcheck_start_period_s, run_on_deploy,
	depends_on, files, storage,
	attached_tile_id, mount_path, volume_name, max_size_mb,
	engine, db_name, db_user, db_password, external_port,
	cron, command, allow_overlap, timeout_minutes, last_run_at, last_status, last_output,
	scope_kind, scope_id, endpoint_protocol, endpoint_port_var, shared_net,
	home_node, replicas, node_group,
	created_at, updated_at`

func (s *Store) CreateTile(ctx context.Context, t *repo.Tile) error {
	enc := *t
	enc.Env = secrets.Encrypt(t.Env)
	if enc.ScopeKind == "" {
		enc.ScopeKind = "env"
	}
	// A zero-value struct means "the caller did not say", not "no replicas".
	if enc.Replicas < 1 {
		enc.Replicas = 1
	}
	if enc.EndpointProtocol == "" {
		enc.EndpointProtocol = "http"
	}
	if enc.UpdatePolicy == "" {
		enc.UpdatePolicy = "off"
	}
	_, err := s.db.NamedExecContext(ctx,
		`INSERT INTO tiles (`+tileCols+`)
		 VALUES (:id, :stack_id, :environment_id, :name, :slug, :kind, :source_type, :git_url, :git_branch, :connector_id,
		 :image_ref, :dockerfile_path, :build_context, :env, :build_args, :volumes,
		 :container_port, :healthcheck_cmd, :webhook_token, :status, :cpu_limit, :mem_limit_mb,
		 :basic_auth_user, :basic_auth_hash, :sec_headers, :published_ports, :traefik_override, :watch_paths,
		 :update_policy, :image_digest, :latest_digest, :wait_for_ci,
		 :user, :shm_size_mb, :privileged, :devices, :restart_policy,
		 :healthcheck_interval_s, :healthcheck_timeout_s, :healthcheck_retries, :healthcheck_start_period_s, :run_on_deploy,
		 :depends_on, :files, :storage,
		 :attached_tile_id, :mount_path, :volume_name, :max_size_mb,
		 :engine, :db_name, :db_user, :db_password, :external_port,
		 :cron, :command, :allow_overlap, :timeout_minutes, :last_run_at, :last_status, :last_output,
		 :scope_kind, :scope_id, :endpoint_protocol, :endpoint_port_var, :shared_net,
		 :home_node, :replicas, :node_group,
		 :created_at, :updated_at)`, &enc)
	if err != nil {
		return err
	}
	return s.projectEnvVars(ctx, t)
}

// projectEnvVars mirrors the tile's env blob into `variables` rows. The blob is
// still the write surface for the web forms, the API and config apply, but the
// resolver reads rows only, projecting here covers every writer at once
// instead of patching five call sites that all do parse→mutate→write.
//
// Additive on purpose. Once R5's API writes structured rows directly, any
// unrelated UpdateTile (a settings save, a clone) carries a blob that never had
// those names in it, deleting what the blob lacks would eat them.
//
// delete this once R7 makes structured rows the write surface; the
// blob column goes with it.
func (s *Store) projectEnvVars(ctx context.Context, t *repo.Tile) error {
	now := time.Now().UTC()
	for _, v := range envutil.Parse(t.Env) {
		if v.Key == "" {
			continue
		}
		if err := s.UpsertVariable(ctx, &repo.Variable{OwnerKind: repo.OwnerTile, OwnerID: t.ID,
			Name: v.Key, Value: v.Value, CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
	}
	return nil
}

// ReplaceTileVars projects the blob and drops every non-secret row the blob
// doesn't declare. Only config apply calls it: the file is the whole truth for
// a managed tile, so a variable deleted from the file has to disappear from the
// resolver's view too, that's the drift the config lock exists to prevent.
// Secret rows survive; they never came from the blob.
//
// goes away with projectEnvVars at R7.
func (s *Store) ReplaceTileVars(ctx context.Context, t *repo.Tile) error {
	if err := s.projectEnvVars(ctx, t); err != nil {
		return err
	}
	declared := map[string]bool{}
	for _, v := range envutil.Parse(t.Env) {
		declared[v.Key] = true
	}
	cur, err := s.ListVariables(ctx, repo.OwnerTile, t.ID)
	if err != nil {
		return err
	}
	for _, v := range cur {
		if v.Secret || declared[v.Name] {
			continue
		}
		if err := s.DeleteVariable(ctx, repo.OwnerTile, t.ID, v.Name); err != nil {
			return err
		}
	}
	return nil
}

func decryptTile(t *repo.Tile, err error) (*repo.Tile, error) {
	if err != nil || t == nil {
		return t, err
	}
	t.Env, err = secrets.Decrypt(t.Env)
	return t, err
}

func decryptTiles(ts []repo.Tile, err error) ([]repo.Tile, error) {
	if err != nil {
		return ts, err
	}
	for i := range ts {
		if ts[i].Env, err = secrets.Decrypt(ts[i].Env); err != nil {
			return ts, err
		}
	}
	return ts, nil
}

func (s *Store) GetTile(ctx context.Context, id string) (*repo.Tile, error) {
	return decryptTile(get[repo.Tile](ctx, s, `SELECT * FROM tiles WHERE id = ?`, id))
}

func (s *Store) GetTileBySlug(ctx context.Context, envID, slug string) (*repo.Tile, error) {
	return decryptTile(get[repo.Tile](ctx, s, `SELECT * FROM tiles WHERE environment_id = ? AND slug = ?`, envID, slug))
}

func (s *Store) ListTilesByEnv(ctx context.Context, envID string) ([]repo.Tile, error) {
	return decryptTiles(list[repo.Tile](ctx, s, `SELECT * FROM tiles WHERE environment_id = ? ORDER BY name`, envID))
}

func (s *Store) ListTilesByStack(ctx context.Context, stackID string) ([]repo.Tile, error) {
	return decryptTiles(list[repo.Tile](ctx, s, `SELECT * FROM tiles WHERE stack_id = ? ORDER BY name`, stackID))
}

func (s *Store) ListTiles(ctx context.Context) ([]repo.Tile, error) {
	return decryptTiles(list[repo.Tile](ctx, s, `SELECT * FROM tiles ORDER BY name`))
}

// UpdateTile writes the whole editable row. Deliberately NOT status: a
// settings save carries whatever status the form was rendered with, so
// saving after a deploy finished used to revert the tile to its old state.
// Status moves only through UpdateTileStatus/RecordTileRun.
//
// shared_net and home_node are out for the same reason: both are infrastructure
// facts the panel discovers (which db-pool overlay the instance holds, which
// node its volume is on), not fields any form renders, so a settings save would
// write back whatever stale value it was rendered with. They move only through
// SetTileSharedNet and SetTileHomeNode.
// RenameTile writes the two columns UpdateTile deliberately leaves alone.
//
// Slug is not in UpdateTile for the same reason status is not: it is identity,
// not settings, and a general save carrying a stale copy of it would rename
// the tile behind whoever asked. The cost of that was that the config engine's
// moved: entries set t.Slug and called UpdateTile, so a declared rename tore
// the service down and then wrote nothing at all.
func (s *Store) RenameTile(ctx context.Context, id, name, slug string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE tiles SET name = ?, slug = ?, updated_at = ? WHERE id = ?`,
		name, slug, time.Now().UTC(), id)
	return err
}

func (s *Store) UpdateTile(ctx context.Context, t *repo.Tile) error {
	t.UpdatedAt = time.Now().UTC()
	enc := *t
	enc.Env = secrets.Encrypt(t.Env)
	_, err := s.db.NamedExecContext(ctx,
		`UPDATE tiles SET name = :name, source_type = :source_type, git_url = :git_url,
		 git_branch = :git_branch, connector_id = :connector_id, image_ref = :image_ref,
		 dockerfile_path = :dockerfile_path, build_context = :build_context,
		 env = :env, build_args = :build_args, volumes = :volumes, container_port = :container_port,
		 healthcheck_cmd = :healthcheck_cmd,
		 updated_at = :updated_at,
		 engine = :engine, db_name = :db_name, db_user = :db_user, db_password = :db_password,
		 external_port = :external_port,
		 cron = :cron, command = :command, allow_overlap = :allow_overlap, timeout_minutes = :timeout_minutes,
		 cpu_limit = :cpu_limit, mem_limit_mb = :mem_limit_mb,
		 basic_auth_user = :basic_auth_user, basic_auth_hash = :basic_auth_hash, sec_headers = :sec_headers,
		 published_ports = :published_ports, traefik_override = :traefik_override, watch_paths = :watch_paths,
		 update_policy = :update_policy, wait_for_ci = :wait_for_ci,
		 user = :user, shm_size_mb = :shm_size_mb, privileged = :privileged, devices = :devices,
		 restart_policy = :restart_policy,
		 healthcheck_interval_s = :healthcheck_interval_s, healthcheck_timeout_s = :healthcheck_timeout_s,
		 healthcheck_retries = :healthcheck_retries, healthcheck_start_period_s = :healthcheck_start_period_s,
		 run_on_deploy = :run_on_deploy, depends_on = :depends_on, files = :files, storage = :storage,
		 attached_tile_id = :attached_tile_id, mount_path = :mount_path, volume_name = :volume_name,
		 max_size_mb = :max_size_mb,
		 scope_kind = :scope_kind, scope_id = :scope_id,
		 endpoint_protocol = :endpoint_protocol, endpoint_port_var = :endpoint_port_var,
		 replicas = :replicas, node_group = :node_group
		 WHERE id = :id`, &enc)
	if err != nil {
		return err
	}
	return s.projectEnvVars(ctx, t)
}

// SetTileHomeNode records which swarm node a pinned tile's volume lives on.
// Deliberately its own writer and absent from UpdateTile: the home node is
// state, set on first deploy and changed only by a completed volume move, so
// an unrelated settings save must never carry a stale value over it. Losing
// the assignment is how swarm schedules a database onto an empty volume
// (docs/plans/30-docker-swarm.md, addendum).
func (s *Store) SetTileHomeNode(ctx context.Context, id, nodeID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE tiles SET home_node = ? WHERE id = ?`, nodeID, id)
	return err
}

// SetTileImageDigest records what a deploy actually pulled. Deliberately not
// part of UpdateTile: digests are runtime state like status, a settings save
// must never carry them.
func (s *Store) SetTileImageDigest(ctx context.Context, id, digest string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE tiles SET image_digest = ? WHERE id = ?`, digest, id)
	return err
}

// SetTileLatestDigest records the newest digest the registry watcher has seen.
func (s *Store) SetTileLatestDigest(ctx context.Context, id, digest string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE tiles SET latest_digest = ? WHERE id = ?`, digest, id)
	return err
}

// RecordTileRun stores the outcome of one cron-kind tile run on the tile row.
//
// tiles.status is deliberately untouched. It is the tile's lifecycle (idle,
// building, error from a deploy), and every card that rolls tiles up takes the
// worst status in the group (graph.WorstStatus), so writing a failed run there
// reddened the whole environment and stack card for one bad tick. The run
// outcome lives in last_status/last_output, which is what the cron card's
// last-run line and the runs list read.
func (s *Store) RecordTileRun(ctx context.Context, id, status, output string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE tiles SET last_run_at = ?, last_status = ?, last_output = ?,
		 updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		time.Now().UTC(), status, output, id)
	return err
}

func (s *Store) UpdateTileStatus(ctx context.Context, id, status string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE tiles SET status = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, status, id)
	return err
}

func (s *Store) DeleteTile(ctx context.Context, id string) error {
	// Variables and bindings go with the tile, no FKs here, and a reused id
	// would otherwise inherit the dead tile's values and grants.
	for _, q := range []string{
		`DELETE FROM variables WHERE owner_kind = 'tile' AND owner_id = ?`,
		`DELETE FROM resource_bindings WHERE consumer_tile_id = ?`,
		`DELETE FROM tiles WHERE id = ?`,
	} {
		if _, err := s.db.ExecContext(ctx, q, id); err != nil {
			return err
		}
	}
	return nil
}
