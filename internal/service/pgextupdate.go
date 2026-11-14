package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
)

// pgextupdKey is an instance's extension update check: pgextupd.<instance id>,
// JSON of PGExtUpdates. No row = never checked.
const pgextupdKey = "pgextupd."

// PGExtChange is one package a newer build exists for: From is what the
// instance has, To what a rebuild would install.
type PGExtChange struct {
	Package string `json:"package"`
	From    string `json:"from"`
	To      string `json:"to"`
}

// PGExtUpdates is the last update check of a recipe-built instance. Empty
// Changes = up to date. Ref is the build the check read: a check of an older
// build is never shown.
type PGExtUpdates struct {
	CheckedAt time.Time     `json:"checked_at"`
	Ref       string        `json:"ref,omitempty"`
	Changes   []PGExtChange `json:"changes"`
}

// PGExtUpdatesOf is the pending extension updates of an instance: nil when
// none were found, it was never checked, or the build moved since the check.
func (o *Orchestrator) PGExtUpdatesOf(ctx context.Context, m OperatedInstance) *PGExtUpdates {
	v, ok, err := o.store.Settings.Get(ctx, pgextupdKey+m.ID)
	if err != nil || !ok {
		return nil
	}
	var u PGExtUpdates
	if json.Unmarshal([]byte(v), &u) != nil || len(u.Changes) == 0 {
		return nil
	}
	if eb, _ := mflow.ParseExtBuild(m.ExtBuild); !eb.Built() || eb.Ref != u.Ref {
		return nil
	}
	return &u
}

// PGExtUpdatesFor is PGExtUpdatesOf by tile id, for the drawer's stream.
func (o *Orchestrator) PGExtUpdatesFor(ctx context.Context, tileID string) *PGExtUpdates {
	if _, m, err := o.instanceOf(ctx, tileID); err == nil {
		return o.PGExtUpdatesOf(ctx, m)
	}
	return nil
}

// pgExtUpdateCount is how many packages have an update, for the canvas card.
func (o *Orchestrator) pgExtUpdateCount(ctx context.Context, m OperatedInstance) int {
	if u := o.PGExtUpdatesOf(ctx, m); u != nil {
		return len(u.Changes)
	}
	return 0
}

func (o *Orchestrator) delPGExtUpdates(ctx context.Context, instanceID string) {
	if err := o.store.Settings.Delete(ctx, pgextupdKey+instanceID); err != nil {
		slog.Warn("pg extension updates: not cleared", "instance", instanceID, "err", err)
	}
}

// checkPGExtUpdates is the daily check of every recipe-built instance: the
// resolve step of a plan runs in the instance's base image on its node and
// the newest installable builds are set against the build record. Nothing is
// built and no plan stored. Instances on the same node, base, major and
// package set share one resolve. A failed resolve keeps the last result. Rows
// of instances that are gone or no longer built are dropped.
func (o *Orchestrator) checkPGExtUpdates(ctx context.Context) {
	ms, err := o.operated.WithEngine(ctx, "postgres")
	if err != nil {
		slog.Warn("pg extension updates: list", "err", err)
		return
	}
	live := map[string]bool{}
	shared := map[string]*mflow.Resolved{} // nil = the resolve failed
	for _, m := range ms {
		eb, _ := mflow.ParseExtBuild(m.ExtBuild)
		if !eb.Built() {
			continue
		}
		live[m.ID] = true
		if err := o.checkPGExtUpdate(ctx, m, eb, shared); err != nil {
			slog.Warn("pg extension updates: check", "instance", m.ID, "err", err)
		}
	}
	rows, err := o.store.Settings.ListPrefix(ctx, pgextupdKey)
	if err != nil {
		return
	}
	for k := range rows {
		if id := strings.TrimPrefix(k, pgextupdKey); !live[id] {
			o.delPGExtUpdates(ctx, id)
		}
	}
}

func (o *Orchestrator) checkPGExtUpdate(ctx context.Context, m OperatedInstance, eb mflow.ExtBuild, shared map[string]*mflow.Resolved) error {
	t, err := o.tiles.Get(ctx, m.TileID)
	if err != nil {
		return err
	}
	node, err := o.envNode(ctx, t.EnvironmentID)
	if err != nil {
		return err
	}
	rs, err := o.pgResolve(ctx, t, m, eb.Recipes)
	if err != nil {
		return err
	}
	changes := []PGExtChange{}
	if pkgs := mflow.RecipePackages(rs, m.Major); len(pkgs) > 0 {
		key := fmt.Sprintf("%s\x00%s\x00%d\x00%s", node, eb.Base, m.Major, strings.Join(pkgs, " "))
		res, seen := shared[key]
		if !seen {
			r, err := o.pgResolveOn(ctx, node, eb.Base, m.ID, m.Major, pkgs)
			if err != nil {
				shared[key] = nil
				return err
			}
			res = &r
			shared[key] = res
		}
		if res == nil {
			return fmt.Errorf("resolve on %s failed", eb.Base)
		}
		for _, p := range pkgs {
			if to, from := res.Versions[p], eb.Packages[p]; to != "" && from != "" && to != from {
				changes = append(changes, PGExtChange{Package: p, From: from, To: to})
			}
		}
	}
	// URL packages: the recipe as it is now against the release the build holds
	urls, err := mflow.URLPackages(rs, m.Major, eb.Arch)
	if err != nil {
		return err
	}
	for p, u := range urls {
		if old, ok := eb.URLs[p]; ok && old.Version != u.Version {
			changes = append(changes, PGExtChange{Package: p, From: old.Version, To: u.Version})
		}
	}
	slices.SortFunc(changes, func(a, b PGExtChange) int { return strings.Compare(a.Package, b.Package) })
	b, _ := json.Marshal(PGExtUpdates{CheckedAt: time.Now().UTC(), Ref: eb.Ref, Changes: changes})
	return o.store.Settings.Set(ctx, pgextupdKey+m.ID, string(b))
}
