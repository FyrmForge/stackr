// Package envops holds environment lifecycle operations shared by the web UI
// and automation triggers (PR webhooks): config-only cloning and full
// teardown. clone-with-data (db dump/restore, volume copy) is the
// planned follow-up and slots in here.
package envops

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/cluster"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/envnet"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/managedtiles"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/netpool"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	svcproxy "github.com/FyrmForge/stackr/internal/stackrd/service/proxy"
	"github.com/FyrmForge/stackr/internal/stackrd/service/scheduler"
	"github.com/FyrmForge/stackr/internal/stackrd/store/audit"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Ops bundles the dependencies environment operations need.
type Ops struct {
	Store repo.Store
	RT    *runtime.Runtime
	// Cluster is what the managed-tiles service is built from. Required on
	// every path that provisions or tears down: a nil one used to fall back
	// to the local socket, silently (docs/plans/35-cluster.md).
	Cluster *cluster.Cluster
	PX      *svcproxy.Service
	// DBs lets Teardown reclaim the slices an ephemeral env provisioned.
	// Optional: the paths that only clone or route leave it nil.
	DBs *managedtiles.Service
	// Tiles is the tile service, so a config apply drops a tile exactly the
	// way the panel and the API do — the provision orphaning and the two
	// schedule reloads included, both of which this path used to skip.
	// Optional: nil falls back to the row delete, which is what tests want.
	Tiles *service.TileService
	// Domains owns the generated hostname a cloned tile inherits, and every
	// rule about what a hostname may be.
	Domains *service.DomainService
	// Resources owns the hostnames names are generated under, including the
	// ACME account each one's certificates are issued on.
	Resources *service.DomainResourceService
	// Envs owns the environments table. Every path here that created,
	// renamed, deleted or re-pointed an environment used to write the row
	// itself, which is how the config applier ended up skipping the reserved
	// -slug and duplicate-name checks the other four creators run.
	Envs *service.EnvironmentService
	// Vars owns the variables table, for the same reason: cloning an env
	// copied its variables with a raw upsert, so none of the secret auditing
	// or the waiting-value clearing happened on that path.
	Vars *service.VariableService
	// Sched re-registers the cron and backup tables. Tearing an env down
	// deletes the environment row, which cascades every tile in it and with
	// them their cron_jobs and backups rows — the same stale-entry bug point
	// 1 closed at the tile delete sites, one level up. Nil-safe.
	Sched *scheduler.Service
}

// CloneTiles copies every tile of env.BaseEnvID into env: same config, fresh
// identity (id, webhook token, db password). Sibling references survive: any
// env var mentioning a base db tile's password is rewritten to the clone's
// new password (hosts stay the same, slug aliases are per-env-network).
// Config-only, domains are not copied (hosts are globally unique) and no
// containers are started.
func (o Ops) CloneTiles(ctx context.Context, env *repo.Environment) error {
	tiles, err := o.Store.ListTilesByEnv(ctx, env.BaseEnvID)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	// Regenerate db credentials first so service tiles can be rewritten.
	rewrite := map[string]string{} // old password -> new password
	idMap := map[string]string{}   // base tile id -> cloned tile id
	baseID := map[string]string{}  // cloned tile id -> base tile id
	for i := range tiles {
		t := &tiles[i]
		newID := uuid.New().String()
		idMap[t.ID] = newID
		baseID[newID] = t.ID
		t.ID = newID
		t.EnvironmentID = env.ID
		t.WebhookToken = uuid.New().String()
		t.Status = "idle"
		t.CreatedAt = now
		t.UpdatedAt = now
		t.LastRunAt = sql.NullTime{}
		t.LastStatus = ""
		t.LastOutput = ""
		// Both belong to the environment the tile came from, not to the tile.
		// SharedNetName carried a PR preview onto production's database
		// network; HomeNode pinned the copy to the original's machine, whose
		// volume it does not have. Placement and netpool fill them in on the
		// first deploy in the new env.
		t.SharedNetName = ""
		t.HomeNode = ""
		if t.IsManaged() {
			old := t.DBPassword
			if err := managedtiles.NewDB(t); err != nil {
				return err
			}
			if old != "" {
				rewrite[old] = t.DBPassword
			}
		}
	}
	for i := range tiles {
		t := &tiles[i]
		for old, new_ := range rewrite {
			t.Env = strings.ReplaceAll(t.Env, old, new_)
		}
		if t.IsVolume() {
			// Fresh empty volume attached to the cloned counterpart, clones
			// never share the base env's data.
			t.VolumeName = ""
			t.AttachedTileID = idMap[t.AttachedTileID]
		}
		if err := o.Store.CreateTile(ctx, t); err != nil {
			return err
		}
		if t.IsManaged() {
			managedtiles.PublishConnection(ctx, o.Store, t)
		}
		// Auto domains are opt-in now: the clone inherits the intent only if
		// the base tile carried a generated hostname.
		if base, err := o.Store.ListDomainsByTile(ctx, baseID[t.ID]); err == nil {
			for _, d := range base {
				if d.Auto {
					if err := o.Domains.EnsureAuto(ctx, env, t); err != nil {
						return err
					}
					break
				}
			}
		}
		if err := o.cloneVars(ctx, baseID[t.ID], t.ID, rewrite, now); err != nil {
			return err
		}
	}
	// Provisioned slices are per-consumer: the clone gets its own database or
	// bucket with its own credentials, never a second consumer of the base
	// env's data.
	for i := range tiles {
		o.cloneProvisions(ctx, &tiles[i], baseID[tiles[i].ID])
	}
	o.cloneEnvSlices(ctx, env, tiles)
	CopyLayout(ctx, o.Store, env.BaseEnvID, env.ID)
	return nil
}

// CopyLayout copies one environment's saved card positions onto another's
// canvas. Env layouts are slug-keyed (clones keep slugs), so rows transfer
// verbatim; uuid-keyed nodes (resources, jobs, port forwards) would point
// into the source env, so they're skipped and those cards auto-place.
// Best-effort, a layout is cosmetic and must never fail a clone.
func CopyLayout(ctx context.Context, store repo.Store, fromEnvID, toEnvID string) {
	if fromEnvID == "" || fromEnvID == toEnvID {
		return
	}
	rows, err := store.ListNodePositions(ctx, repo.GraphOwner(repo.ScopeEnv, fromEnvID))
	if err != nil {
		return
	}
	kept := rows[:0]
	for _, r := range rows {
		kind, _, _ := strings.Cut(r.NodeID, ":")
		if kind == "resource" || kind == "job" || kind == "forward" {
			continue
		}
		kept = append(kept, r)
	}
	if len(kept) == 0 {
		return
	}
	if err := store.SaveNodePositions(ctx, repo.GraphOwner(repo.ScopeEnv, toEnvID), kept); err != nil {
		slog.Warn("layout copy failed", "from", fromEnvID, "to", toEnvID, "error", err)
	}
}

// cloneEnvSlices copies the base env's config-declared slices (env-level, no
// consumer row) into the clone under the same reference slug, then binds the
// cloned tiles that reference them. Fresh data, same names, the config's refs
// keep resolving without any rewriting.
//
// Best-effort like cloneProvisions: an unreachable instance leaves a reference
// that fails loudly at deploy rather than sharing the base env's data.
func (o Ops) cloneEnvSlices(ctx context.Context, env *repo.Environment, cloned []repo.Tile) {
	ps, err := o.Store.ListProvisionsByEnv(ctx, env.BaseEnvID)
	if err != nil || len(ps) == 0 {
		return
	}
	svc := managedtiles.NewService(o.Cluster, o.Store)
	slugs := map[string]bool{}
	for i := range ps {
		p := &ps[i]
		if p.ConsumerTileID != "" || p.Status == "orphaned" || p.ResourceSlug == "" {
			continue
		}
		inst, err := o.Store.GetTile(ctx, p.InstanceTileID)
		if err != nil || inst == nil {
			continue
		}
		if _, err := svc.CloneSlice(ctx, inst, env.ID, p.ResourceSlug, p.DBName, p.Public); err != nil {
			slog.Error("clone slice failed", "slice", p.ResourceSlug, "instance", inst.Slug, "error", err)
			continue
		}
		slugs[p.ResourceSlug] = true
	}
	if len(slugs) == 0 {
		return
	}
	res, err := o.Store.ListResourcesByEnv(ctx, env.ID)
	if err != nil {
		return
	}
	now := time.Now().UTC()
	for i := range cloned {
		t := &cloned[i]
		for j := range res {
			if slugs[res[j].Slug] && strings.Contains(t.Env, "tile."+res[j].Slug+".") {
				if err := o.Store.CreateBinding(ctx, &repo.ResourceBinding{ResourceID: res[j].ID,
					ConsumerTileID: t.ID, CreatedAt: now}); err != nil {
					slog.Error("cloned resource binding not saved", "resource", res[j].ID, "tile", t.ID, "error", err)
				}
			}
		}
	}
}

// cloneVars copies a tile's variable definitions, including secrets, the env
// blob only carries the non-secret ones, so copying the tile row alone would
// silently drop every secret the app needs.
func (o Ops) cloneVars(ctx context.Context, baseTileID, newTileID string, rewrite map[string]string, now time.Time) error {
	if baseTileID == "" {
		return nil
	}
	vars, err := o.Store.ListVariables(ctx, repo.OwnerTile, baseTileID)
	if err != nil {
		return err
	}
	for _, v := range vars {
		for old, new_ := range rewrite {
			v.Value = strings.ReplaceAll(v.Value, old, new_)
		}
		v.OwnerID, v.CreatedAt, v.UpdatedAt = newTileID, now, now
		if err := o.Vars.Upsert(ctx, &v); err != nil {
			return err
		}
		if v.Secret {
			// A clone copies every secret the base tile holds onto a new
			// owner. That is a second place the plaintext now lives, and
			// nothing recorded it happening.
			audit.Record(ctx, o.Store, "system:env-clone", audit.Set, repo.OwnerTile, newTileID, v.Name)
		}
	}
	return nil
}

// cloneProvisions gives the cloned consumer its own logical database/bucket
// from the same instance and repoints its references at the new resource.
//
// Best-effort: provisioning needs the instance running, and a clone that mostly
// works beats a clone that fails outright. A reference left pointing at the
// base env's resource fails loudly at deploy, the resolver refuses an unbound
// resource, rather than quietly sharing another environment's data.
func (o Ops) cloneProvisions(ctx context.Context, clone *repo.Tile, baseTileID string) {
	if baseTileID == "" || clone.IsManaged() || clone.IsVolume() {
		return
	}
	ps, err := o.Store.ListProvisionsByConsumer(ctx, baseTileID)
	if err != nil || len(ps) == 0 {
		return
	}
	svc := managedtiles.NewService(o.Cluster, o.Store)
	for i := range ps {
		inst, err := o.Store.GetTile(ctx, ps[i].InstanceTileID)
		if err != nil || inst == nil {
			continue
		}
		fresh, err := svc.Provision(ctx, inst, clone, "", ps[i].Public)
		if err != nil {
			slog.Error("clone provision failed", "tile", clone.Slug, "instance", inst.Slug, "error", err)
			continue
		}
		o.repointRefs(ctx, clone.ID, managedtiles.ResourceSlug(inst, &ps[i]), managedtiles.ResourceSlug(inst, fresh))
	}
}

// repointRefs rewrites a tile's references from one resource slug to another.
func (o Ops) repointRefs(ctx context.Context, tileID, oldSlug, newSlug string) {
	if oldSlug == newSlug {
		return
	}
	vars, err := o.Store.ListVariables(ctx, repo.OwnerTile, tileID)
	if err != nil {
		return
	}
	for _, v := range vars {
		updated := strings.ReplaceAll(v.Value, "tile."+oldSlug+".", "tile."+newSlug+".")
		if updated == v.Value {
			continue
		}
		v.Value, v.UpdatedAt = updated, time.Now().UTC()
		_ = o.Vars.Upsert(ctx, &v)
	}
}

// Teardown removes an environment's containers, proxy routes, and network,
// then its rows (tiles cascade). DB volumes are kept, matching single-db
// deletes.
//
// RT and PX are treated as optional here, the same way DBs already is: every
// call through them is best-effort (the errors are discarded, a container that
// is already gone must not strand the rows), so skipping them when there is no
// docker to talk to changes nothing for the server and lets the row-level half
// of teardown (slice reclaim and the variable cleanup below) be tested
// without one.
func (o Ops) Teardown(ctx context.Context, stack *repo.Stack, env *repo.Environment) error {
	tiles, err := o.Store.ListTilesByEnv(ctx, env.ID)
	if err != nil {
		return err
	}
	for _, t := range tiles {
		envnet.TearDown(ctx, o.Store, o.Cluster, &t)
		o.PX.DropTile(t.ID)
		o.reclaimSlices(ctx, env, &t)
	}
	// Env-level (config-declared) slices have no consumer tile, so the per-tile
	// reclaim above never sees them.
	if o.DBs != nil {
		if ps, err := o.Store.ListProvisionsByEnv(ctx, env.ID); err == nil {
			for i := range ps {
				p := &ps[i]
				if p.ConsumerTileID != "" || p.Status == "orphaned" {
					continue
				}
				if env.Type != "ephemeral" {
					_ = o.DBs.Detach(ctx, p)
					continue
				}
				if inst, gerr := o.Store.GetTile(ctx, p.InstanceTileID); gerr == nil && inst != nil {
					_ = o.DBs.DropDB(ctx, inst, p.DBName)
					continue
				}
				if err := o.Store.DeleteProvision(ctx, p.ID); err != nil {
					slog.Error("provision row not deleted", "provision", p.ID, "error", err)
				}
			}
		}
	}
	// Variables have no FK to environments, a torn-down env's rows (including
	// per-PR generated secrets) must go explicitly or they leak forever.
	if vars, err := o.Store.ListVariables(ctx, repo.OwnerEnv, env.ID); err == nil {
		for _, v := range vars {
			if err := o.Vars.Remove(ctx, service.EnvVars(env.ID), v.Name); err != nil {
				slog.Error("env variable not deleted", "env", env.ID, "name", v.Name, "error", err)
			}
		}
	}
	// Pooled overlays, handed back by name rather than removed. Both releases
	// gate the row delete: the pool's free list is computed from live rows
	// (store.ClaimedNetworks), so deleting the row while the network still
	// has the old tenant's services on it hands that network, with those
	// services, to whatever claims next, possibly another org. Nothing
	// self-heals, netpool.Sweep runs at boot only.
	//
	// Retry is delete again: every step above is idempotent on an
	// already-empty env, ReleaseDB returns nil once SharedNetName is cleared
	// and ReleaseEnv once env.Network is.
	if o.RT != nil {
		// A second pass, after every service is gone. drain detaches every
		// service on the network, so releasing a shared instance's overlay
		// while its consumer tiles are still up would detach services that
		// still exist. An org-scoped instance is skipped: it is hosted in
		// this env but shared across the org's stacks, and orgconf owns it.
		for i := range tiles {
			t := &tiles[i]
			if t.SharedNetName == "" || t.ScopeKind == "org" {
				continue
			}
			if err := netpool.ReleaseDB(ctx, o.Store, o.RT, t); err != nil {
				return fmt.Errorf("releasing overlay %s of %s: %w", t.SharedNetName, t.Slug, err)
			}
		}
		if err := netpool.ReleaseEnv(ctx, o.Store, o.RT, env); err != nil {
			return fmt.Errorf("releasing overlay %s: %w", env.Network, err)
		}
	}
	if err := o.Envs.Remove(ctx, env.ID); err != nil {
		return err
	}
	// The cascade took this env's cron_jobs and backups rows with it; without
	// the reload the orphaned entries keep ticking against tiles that are gone.
	o.Sched.Reload(ctx)
	return nil
}

// TeardownStack tears every environment of a stack down, which is what has to
// happen before the stack row goes: DeleteStack cascades the rows, and a
// pooled overlay is only ever freed by name, so a plain cascade left every
// environment's network claimed forever and its services running on a network
// nothing owned.
func (o Ops) TeardownStack(ctx context.Context, stack *repo.Stack) error {
	envs, err := o.Store.ListEnvironmentsByStack(ctx, stack.ID)
	if err != nil {
		return err
	}
	for i := range envs {
		if err := o.Teardown(ctx, stack, &envs[i]); err != nil {
			return err
		}
	}
	return nil
}

// reclaimSlices releases the shared-instance slices a torn-down tile held.
//
// An ephemeral env's slices are dropped outright, whatever on_remove says: a PR
// env's logical database has no life after the PR, and provisions carry no
// foreign key, so leaving them would leak both a row pointing at a deleted tile
// and a database inside the shared instance that nothing will ever reclaim.
// A static env detaches instead, keeping the data, the same rule the config
// engine's tile deletes follow.
func (o Ops) reclaimSlices(ctx context.Context, env *repo.Environment, t *repo.Tile) {
	if o.DBs == nil {
		return
	}
	ps, err := o.Store.ListProvisionsByConsumer(ctx, t.ID)
	if err != nil {
		return
	}
	for i := range ps {
		p := &ps[i]
		if env.Type != "ephemeral" {
			_ = o.DBs.Detach(ctx, p)
			continue
		}
		if inst, gerr := o.Store.GetTile(ctx, p.InstanceTileID); gerr == nil && inst != nil {
			_ = o.DBs.DropDB(ctx, inst, p.DBName)
			continue
		}
		if err := o.Store.DeleteProvision(ctx, p.ID); err != nil {
			slog.Error("provision row not deleted", "provision", p.ID, "error", err)
		}
	}
}
