// Package envops holds environment lifecycle operations shared by the web UI
// and automation triggers (PR webhooks): config-only cloning and full
// teardown. clone-with-data (db dump/restore, volume copy) is the
// planned follow-up and slots in here.
package envops

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/cluster"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/envnet"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/managedtiles"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/netpool"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/proxy"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
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
	PX      *proxy.Proxy
	// DBs lets Teardown reclaim the slices an ephemeral env provisioned.
	// Optional: the paths that only clone or route leave it nil.
	DBs *managedtiles.Service
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
					if err := o.EnsureAutoDomain(ctx, env, t); err != nil {
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
		if err := o.Store.UpsertVariable(ctx, &v); err != nil {
			return err
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
		_ = o.Store.UpsertVariable(ctx, &v)
	}
}

// ValidateResourceHost checks a domain-resource host: non-empty, lowercase,
// a bare hostname (no scheme, slash, port or spaces). One rule shared by the
// panel forms and the API so every surface rejects the same inputs.
func ValidateResourceHost(host string) error {
	if host == "" {
		return fmt.Errorf("host required")
	}
	if host != strings.ToLower(host) {
		return fmt.Errorf("host must be lowercase")
	}
	if strings.ContainsAny(host, "/: ") || strings.Contains(host, "//") {
		return fmt.Errorf("host must be a bare hostname; no scheme, slash or port")
	}
	return nil
}

// HostTaken reports whether another resource already owns host (hosts are
// globally unique, the DB index enforces it, this gives a friendly error).
func HostTaken(all []repo.DomainResource, host string) bool {
	for _, r := range all {
		if r.Host == host {
			return true
		}
	}
	return false
}

// CheckOrgSquat rejects a hostname whose first label is another organization's
// slug. Generated hostnames nest under an org's own domains, so a host like
// orgb.example.com claimed by org A can shadow or impersonate org B's
// addresses. Every path that accepts a hostname goes through here: org domain
// resources, tile-level custom domains and config-as-code domain blocks.
func CheckOrgSquat(ctx context.Context, store repo.Store, host, ownOrgID string) error {
	label, _, ok := strings.Cut(strings.TrimPrefix(host, "*."), ".")
	if !ok || label == "" {
		return nil
	}
	other, err := store.GetOrgBySlug(ctx, label)
	if err != nil {
		// A guard against impersonation may not fail open on a store blip.
		return fmt.Errorf("could not check that hostname against organization slugs: %w", err)
	}
	if other == nil || other.ID == ownOrgID {
		return nil
	}
	return fmt.Errorf("that hostname starts with another organization's slug")
}

// AutoHost is the generated hostname for a tile under a domain resource:
// dotted segments truncated at the owning level (a stack-owned base already
// implies the stack, so its segment is dropped), the default env's slug
// omitted unless the resource opts in.
func AutoHost(res repo.DomainResource, orgSlug, stackSlug, envSlug, tileSlug string, isDefaultEnv bool) string {
	segs := []string{tileSlug}
	if !isDefaultEnv || res.IncludeEnvOnDefault {
		segs = append(segs, envSlug)
	}
	switch res.Level {
	case "stack":
	case "org":
		segs = append(segs, stackSlug)
	default: // instance
		segs = append(segs, stackSlug, orgSlug)
	}
	return strings.Join(segs, ".") + "." + res.Host
}

// VisibleDomainResources filters to what a stack may claim under, its own,
// its org's, the instance's, nearest level first, then a declared resource
// ahead of an undeclared one, oldest first within that.
//
// Declared wins because the wizard hands an org with no domain of its own a
// default under the server's hostname, so its stacks get names at all. That
// row is older than anything a config file declares later, and oldest-first
// alone would leave the default beating the domain the file actually asked
// for.
func VisibleDomainResources(all []repo.DomainResource, stackID, orgID string) []repo.DomainResource {
	rank := func(r repo.DomainResource) int {
		lvl := 2
		switch r.Level {
		case "stack":
			lvl = 0
		case "org":
			lvl = 1
		}
		if r.Declared {
			return lvl * 2
		}
		return lvl*2 + 1
	}
	var out []repo.DomainResource
	for _, r := range all {
		switch r.Level {
		case "stack":
			if r.OwnerID == stackID {
				out = append(out, r)
			}
		case "org":
			if r.OwnerID == orgID {
				out = append(out, r)
			}
		default: // instance, single server today; every stack sees it
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return rank(out[i]) < rank(out[j]) })
	return out
}

// EnsureAutoDomain gives a service tile its generated hostname under the
// nearest domain resource visible to its stack. Fired on request (config
// `auto: true`, panel/CLI ask, clone of a tile that had one), never
// automatically for every tile. Idempotent; a stale generated row (the
// naming pattern or resource changed) is replaced. HTTPS: per-host Let's
// Encrypt http-challenge, works at any dot depth, no wildcard cert.
func (o Ops) EnsureAutoDomain(ctx context.Context, env *repo.Environment, t *repo.Tile) error {
	if t.IsManaged() || t.Kind != "service" || t.ContainerPort == 0 {
		return nil
	}
	stack, err := o.Store.GetStack(ctx, t.StackID)
	if err != nil || stack == nil {
		return err
	}
	org, err := o.Store.GetOrg(ctx, stack.OrgID)
	if err != nil || org == nil {
		return err
	}
	all, err := o.Store.ListDomainResources(ctx)
	if err != nil {
		return err
	}
	visible := VisibleDomainResources(all, stack.ID, stack.OrgID)
	if len(visible) == 0 {
		return nil // nothing to nest under
	}
	def, err := o.defaultEnvID(ctx, env.StackID)
	if err != nil {
		return err
	}
	host := AutoHost(visible[0], org.Slug, stack.Slug, env.Slug, t.Slug, def == env.ID)
	existing, err := o.Store.ListDomainsByTile(ctx, t.ID)
	if err != nil {
		return err
	}
	kept := existing[:0]
	for _, d := range existing {
		if d.Host == host {
			return nil
		}
		if d.Auto {
			// A generated row under an old pattern; the replacement below
			// carries the current one.
			if err := o.Store.DeleteDomain(ctx, d.ID); err != nil {
				return err
			}
			continue
		}
		kept = append(kept, d)
	}
	d := &repo.Domain{
		ID:            uuid.New().String(),
		TileID:        t.ID,
		Host:          host,
		Path:          "/",
		ContainerPort: t.ContainerPort,
		HTTPS:         true,
		Auto:          true,
		Position:      len(kept),
		CreatedAt:     time.Now().UTC(),
	}
	if err := o.Store.CreateDomain(ctx, d); err != nil {
		return err
	}
	return o.PX.WriteApp(t, append(kept, *d))
}

// defaultEnvID is the stack's default environment: the bottom rung of the
// declared ladder, which is position 0. Its tiles get the bare generated
// hostname with no env segment in it (AutoHost).
//
// It used to be the store's oldest row, which is a different thing and was a
// bug: whichever environment happened to be created first took the bare host,
// even when the config file declares another one as the bottom rung. On a
// stack where production was applied before staging existed, production took
// site.<stack>.<org>.<domain>, and every later plan then failed with "already
// routes to another service" with no way to fix it from the file.
//
// Position is written from the file's environment order on every apply, so it
// is the file's answer. On a stack nobody manages from a file every position
// is 0 and the oldest row wins the tie, which is the old behaviour.
func (o Ops) defaultEnvID(ctx context.Context, stackID string) (string, error) {
	envs, err := o.Store.ListEnvironmentsByStack(ctx, stackID)
	if err != nil || len(envs) == 0 {
		return "", err
	}
	best := -1
	for i := range envs {
		// The home env holds stack-scoped instances, never routed tiles, and
		// it is not on the ladder. It must never be the one that wins.
		if envs[i].Type != "static" || envs[i].Slug == repo.HomeSlug {
			continue
		}
		if best < 0 || envs[i].Position < envs[best].Position {
			best = i
		}
	}
	if best < 0 {
		return envs[0].ID, nil
	}
	return envs[best].ID, nil
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
		if o.PX != nil {
			_ = o.PX.RemoveApp(t.ID)
		}
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
			if err := o.Store.DeleteVariable(ctx, repo.OwnerEnv, env.ID, v.Name); err != nil {
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
	return o.Store.DeleteEnvironment(ctx, env.ID)
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

// ConvertLegacyMounts is the one-shot upgrade to volume tiles: every named
// `name:/path` line on a service becomes an attached volume tile keeping the
// legacy docker volume name (data preserved). Bind-mount paths (starting with
// / or .) stay as text, they're host paths, not volumes. Guarded by a
// settings flag so it runs once.
func ConvertLegacyMounts(ctx context.Context, store repo.Store) error {
	if v, _ := store.GetSetting(ctx, "volumes_migrated"); v == "1" {
		return nil
	}
	tiles, err := store.ListTiles(ctx)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for i := range tiles {
		t := &tiles[i]
		if t.Kind != "service" || t.IsManaged() || strings.TrimSpace(t.Volumes) == "" {
			continue
		}
		var keep []string
		changed := false
		for _, line := range strings.Split(t.Volumes, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			name, path, ok := strings.Cut(line, ":")
			if !ok || name == "" || strings.HasPrefix(name, "/") || strings.HasPrefix(name, ".") {
				keep = append(keep, line)
				continue
			}
			vt := &repo.Tile{
				ID:             uuid.New().String(),
				StackID:        t.StackID,
				EnvironmentID:  t.EnvironmentID,
				Name:           name,
				Slug:           uniqueSlug(ctx, store, t.EnvironmentID, repo.Slugify(name)),
				Kind:           "volume",
				SourceType:     "image",
				AttachedTileID: t.ID,
				MountPath:      path,
				VolumeName:     name, // keep the existing docker volume, data survives
				WebhookToken:   uuid.New().String(),
				Status:         "idle",
				CreatedAt:      now,
				UpdatedAt:      now,
			}
			if err := store.CreateTile(ctx, vt); err != nil {
				return err
			}
			changed = true
		}
		if changed {
			t.Volumes = strings.Join(keep, "\n")
			if err := store.UpdateTile(ctx, t); err != nil {
				return err
			}
		}
	}
	return store.SetSetting(ctx, "volumes_migrated", "1")
}

// uniqueSlug appends -vol (then -vol2…) until the slug is free in the env.
func uniqueSlug(ctx context.Context, store repo.Store, envID, slug string) string {
	candidate := slug
	for i := 0; i < 10; i++ {
		if existing, _ := store.GetTileBySlug(ctx, envID, candidate); existing == nil {
			return candidate
		}
		if i == 0 {
			candidate = slug + "-vol"
		} else {
			candidate = fmt.Sprintf("%s-vol%d", slug, i+1)
		}
	}
	return candidate
}

// PRConfig enables GitHub pull-request environments for one stack. What each
// PR env contains comes from the config file's pr_envs: template, not from a
// base environment, there is nothing to pick here.
type PRConfig struct {
	Enabled bool   `json:"enabled"`
	Secret  string `json:"secret"` // webhook HMAC secret
	// Inverted so the zero value keeps both on for existing stacks.
	NoComment bool `json:"no_comment"` // suppress the sticky PR preview comment
	NoStatus  bool `json:"no_status"`  // suppress the commit status check
}

func prKey(stackID string) string { return "prenv." + stackID }

// LoadPRConfig reads a stack's PR-environment config (zero value if unset).
func LoadPRConfig(ctx context.Context, store repo.Store, stackID string) PRConfig {
	var cfg PRConfig
	if v, err := store.GetSetting(ctx, prKey(stackID)); err == nil && v != "" {
		_ = json.Unmarshal([]byte(v), &cfg)
	}
	return cfg
}

// SavePRConfig persists a stack's PR-environment config.
func SavePRConfig(ctx context.Context, store repo.Store, stackID string, cfg PRConfig) error {
	b, _ := json.Marshal(cfg)
	return store.SetSetting(ctx, prKey(stackID), string(b))
}
