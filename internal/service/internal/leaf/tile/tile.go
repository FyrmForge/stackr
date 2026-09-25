// Package tile owns tiles: the row and its containers (replicas and the
// pause container that holds the tile's VIP). Create, update, rename and
// delete with their refusals (tile-crud, tilelifecycle extracts); one
// whitelist of what each kind may carry (B26, check.go); which keys moved
// and what that earns (diff.go); the containers, the system guard and the
// health gate (world.go). A leaf runs a spec, never builds one.
package tile

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/slug"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type Leaf struct {
	tiles  store.TileStore
	docker Docker
	vip    VIP
	// Gate is the health gate's timing; tests shorten it.
	Gate Gate
}

func New(tiles store.TileStore, d Docker, v VIP) *Leaf {
	return &Leaf{tiles: tiles, docker: d, vip: v, Gate: DefaultGate}
}

func (l *Leaf) Get(ctx context.Context, id string) (store.Tile, error) { return l.tiles.Get(ctx, id) }

func (l *Leaf) GetBySlug(ctx context.Context, envID, slug string) (store.Tile, error) {
	return l.tiles.GetBySlug(ctx, envID, slug)
}

func (l *Leaf) List(ctx context.Context, envID string) ([]store.Tile, error) {
	return l.tiles.ListByEnv(ctx, envID)
}

func (l *Leaf) ListByStack(ctx context.Context, stackID string) ([]store.Tile, error) {
	return l.tiles.ListByStack(ctx, stackID)
}

func (l *Leaf) ListByKind(ctx context.Context, kind string) ([]store.Tile, error) {
	return l.tiles.ListByKind(ctx, kind)
}

// Create is name → defaults → validate → write. A caller may supply the slug
// (the stack file's key); otherwise it comes from the name.
func (l *Leaf) Create(ctx context.Context, t store.Tile) (store.Tile, error) {
	t.Name = strings.TrimSpace(t.Name)
	if t.Name == "" {
		return t, errs.Invalidf("name", "a tile needs a name")
	}
	if t.Slug == "" {
		t.Slug = slug.Make(t.Name)
	}
	if err := l.checkSlug(ctx, t); err != nil {
		return t, err
	}
	now := time.Now().UTC()
	t.ID, t.CreatedAt, t.UpdatedAt = uuid.NewString(), now, now
	if Builds(t) {
		if t.GitBranch == "" {
			t.GitBranch = "main"
		}
		if t.DockerfilePath == "" {
			t.DockerfilePath = "Dockerfile"
		}
		if t.BuildContext == "" {
			t.BuildContext = "."
		}
	}
	if err := Validate(&t); err != nil {
		return t, err
	}
	return t, l.tiles.Create(ctx, t)
}

// checkSlug: derivable, not reserved, free in this environment (not the
// stack, not the org: the slug is the tile's DNS alias on the env network).
func (l *Leaf) checkSlug(ctx context.Context, t store.Tile) error {
	switch {
	case t.Slug == "":
		return errs.Invalidf("name", "a name needs at least one letter or number")
	case !slug.Valid(t.Slug):
		return errs.Invalidf("name", "%q is not a valid slug: lower-case letters, digits and single hyphens", t.Slug)
	case slug.Reserved(t.Slug):
		return errs.Invalidf("name", "%q is reserved for variable references; pick another name", t.Slug)
	}
	holder, err := l.tiles.GetBySlug(ctx, t.EnvironmentID, t.Slug)
	if err == nil && holder.ID != t.ID {
		return errs.Conflictf("a tile named %q (%s) already exists in this environment", holder.Name, t.Slug)
	}
	if err != nil && !errors.Is(err, errs.ErrNotFound) {
		return err
	}
	return nil
}

// Update takes the whole edited row, not a patch, and returns what the write
// earns. Identity (id, stack, env, slug, created_at) is taken from old: only
// Rename moves a slug. extra names changes that are not columns on this row
// ("domain +api.example.com"), so a caller that moved them earns their
// effects too.
func (l *Leaf) Update(ctx context.Context, old, cur store.Tile, extra ...string) (store.Tile, []Effect, error) {
	cur.ID, cur.StackID, cur.EnvironmentID, cur.Slug, cur.CreatedAt =
		old.ID, old.StackID, old.EnvironmentID, old.Slug, old.CreatedAt
	cur.Name = strings.TrimSpace(cur.Name)
	if cur.Name == "" {
		return old, nil, errs.Invalidf("name", "a tile needs a name")
	}
	if err := Validate(&cur); err != nil {
		return old, nil, err
	}
	c := Diff(old, cur)
	for _, k := range extra {
		c[k] = true
	}
	if !c.Any() && cur.Name == old.Name {
		return old, nil, nil
	}
	cur.UpdatedAt = time.Now().UTC()
	return cur, Effects(cur.Kind, c), l.tiles.Update(ctx, cur)
}

// Rename moves name and slug together. Unlike Create it has no "needs a
// name" refusal; an empty name fails on the empty slug (pinned divergence).
// The flow tears the containers down first (their alias is the slug) and
// rewrites the route after; Rename itself does not redeploy.
func (l *Leaf) Rename(ctx context.Context, t store.Tile, name string) (store.Tile, error) {
	t.Name = strings.TrimSpace(name)
	t.Slug = slug.Make(t.Name)
	if err := l.checkSlug(ctx, t); err != nil {
		return t, err
	}
	t.UpdatedAt = time.Now().UTC()
	return t, l.tiles.Update(ctx, t)
}

// Delete removes the row. Containers go first (Teardown), routes and
// provisions are the flow's.
// ponytail: the "detach the volume first" refusal needs the volumes table,
// so it lives in leaf/volume (task 8), not here.
func (l *Leaf) Delete(ctx context.Context, id string) error { return l.tiles.Delete(ctx, id) }
