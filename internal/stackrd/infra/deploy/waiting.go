package deploy

import (
	"context"
	"log/slog"
	"strings"

	"github.com/FyrmForge/stackr/internal/stackrd/config/varref"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// WaitingPrefix marks a tile parked on a value its config declares and nobody
// has set. The status column carries the name so the canvas can say which one
// without a second query per card.
const WaitingPrefix = "waiting:"

// WaitingFor returns the value a tile is parked on, or "" when it is not.
func WaitingFor(status string) string {
	name, _ := strings.CutPrefix(status, WaitingPrefix)
	if name == status {
		return ""
	}
	return name
}

// DepWaiting reports whether any of the tile's dependencies is itself parked
// on a value, as the same UnsetError an unresolved reference produces, so the
// caller's existing waiting branch parks this tile on the same name. Setting
// the value then releases the whole chain at once, because ClearWaiting
// matches on the name and nothing else.
//
// Without it a dependent either goes red after the two-minute depends_on wait
// (:healthy, :completed) or starts blind against a tile that never came up
// (:started). Neither is the truth: it is waiting, same as its dependency.
func DepWaiting(ctx context.Context, store repo.Store, app *repo.Tile) error {
	for _, line := range strings.Split(app.DependsOn, "\n") {
		// Only the slug matters here; the condition cannot be met either way.
		// split rather than import stackconf.ParseDep, stackconf
		// imports this package, so borrowing its parser is an import cycle.
		slug, _, _ := strings.Cut(strings.TrimSpace(line), ":")
		if slug == "" {
			continue
		}
		dep, err := store.GetTileBySlug(ctx, app.EnvironmentID, slug)
		if err != nil || dep == nil {
			continue // a missing dependency is the deploy's problem, not ours
		}
		if name := WaitingFor(dep.Status); name != "" {
			return &varref.UnsetError{
				Source: "tile " + slug + " is waiting",
				Scope:  "stack",
				Name:   name,
			}
		}
	}
	return nil
}

// ClearWaiting releases every tile of the given stacks parked on name, now
// that somebody has set it. The tile goes to "stopped", which is what it
// actually is: nothing is running and the canvas offers Deploy. Deploying for
// them would be a surprise: setting a variable is not a request to ship.
//
// Scoped to stacks because a variable name is not unique across tenants; one
// org setting DATABASE_URL must not touch another org's cards.
func ClearWaiting(ctx context.Context, store repo.Store, name string, stackIDs ...string) {
	if name == "" {
		return
	}
	for _, sid := range stackIDs {
		tiles, err := store.ListTilesByStack(ctx, sid)
		if err != nil {
			continue
		}
		for _, t := range tiles {
			if WaitingFor(t.Status) == name {
				if err := store.UpdateTileStatus(ctx, t.ID, "stopped"); err != nil {
					slog.Error("tile not released from waiting", "tile", t.ID, "waiting_for", name, "error", err)
				}
			}
		}
	}
}

// ClearWaitingEnv releases only the tiles of one environment. An env-scoped
// value satisfies nothing outside its env: the other envs are still parked on
// the same name and have to keep saying so.
func ClearWaitingEnv(ctx context.Context, store repo.Store, envID, name string) {
	if name == "" {
		return
	}
	tiles, err := store.ListTilesByEnv(ctx, envID)
	if err != nil {
		return
	}
	for _, t := range tiles {
		if WaitingFor(t.Status) == name {
			if err := store.UpdateTileStatus(ctx, t.ID, "stopped"); err != nil {
				slog.Error("tile not released from waiting", "tile", t.ID, "waiting_for", name, "error", err)
			}
		}
	}
}

// ClearWaitingOrg is ClearWaiting over every stack in an org, for org-scoped
// variables.
func ClearWaitingOrg(ctx context.Context, store repo.Store, orgID, name string) {
	stacks, err := store.ListStacksByOrg(ctx, orgID)
	if err != nil {
		return
	}
	ids := make([]string, 0, len(stacks))
	for _, s := range stacks {
		ids = append(ids, s.ID)
	}
	ClearWaiting(ctx, store, name, ids...)
}
