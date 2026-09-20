package orgconf

// The org file's `moved:` entries: stack.X and shared.X. Same grammar as the
// stack file's (stackconf/moved.go), scoped to this org, because the file's own
// scope is the namespace and an org file must not be able to declare a move
// somewhere else.

import (
	"context"

	"github.com/FyrmForge/stackr/internal/stackrd/config/stackconf"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/envnet"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// planMoves validates the entries and adds one row each. The moves it returns
// are the ones the apply should perform.
func (r Runner) planMoves(ctx context.Context, org *repo.Org, f *File, p *stackconf.Plan) []stackconf.Move {
	moves, err := stackconf.ParseMoves(f.Moved, "stack", "shared")
	if err != nil {
		p.Errors = append(p.Errors, err.Error())
		return nil
	}
	stacks, _ := r.Store.ListStacksByOrg(ctx, org.ID)
	stackSlugs := map[string]bool{}
	for i := range stacks {
		stackSlugs[stacks[i].Slug] = true
	}
	shared, _ := r.orgTiles(ctx, org)
	sharedSlugs := map[string]bool{}
	for i := range shared {
		sharedSlugs[shared[i].Slug] = true
	}
	var todo []stackconf.Move
	for _, m := range moves {
		st := stackconf.MoveState{}
		if m.Kind == "stack" {
			st.HasFrom, st.HasTo = stackSlugs[m.From], stackSlugs[m.To]
			_, st.Declared = f.Stacks[m.To]
		} else {
			st.HasFrom, st.HasTo = sharedSlugs[m.From], sharedSlugs[m.To]
			_, st.Declared = f.Shared[m.To]
		}
		ch, warn, errMsg := stackconf.PlanMove(m, st)
		switch {
		case errMsg != "":
			p.Errors = append(p.Errors, errMsg)
		case warn != "":
			p.Warnings = append(p.Warnings, warn)
		case ch != nil:
			ch.Env = "org"
			p.Changes = append(p.Changes, *ch)
			todo = append(todo, m)
		}
	}
	return todo
}

// applyMoves performs them. The row keeps its id, so everything hanging off it
// rides along; only the slug and what is derived from it change.
func (r Runner) applyMoves(ctx context.Context, org *repo.Org, f *File) error {
	moves, err := stackconf.ParseMoves(f.Moved, "stack", "shared")
	if err != nil {
		return err
	}
	for _, m := range moves {
		if m.Kind == "stack" {
			s, err := r.Store.GetStackBySlug(ctx, org.ID, m.From)
			if err != nil || s == nil {
				continue // already applied, or nothing to move
			}
			if other, _ := r.Store.GetStackBySlug(ctx, org.ID, m.To); other != nil {
				continue // both exist: a plan error, refused before this ran
			}
			// The stack slug is in every container name under it, so the
			// stack's own apply is what rebuilds them; it runs right after,
			// and it is also what knows the display name the file wants.
			//
			// Reslug, not a rename: this wrote Name and Slug both, from the
			// slug, so a stack called "Billing API" that moved to `billing`
			// came out displayed as "billing".
			if err := r.StackSvc.Reslug(ctx, s, m.To); err != nil {
				return err
			}
			continue
		}
		tiles, err := r.orgTiles(ctx, org)
		if err != nil {
			return err
		}
		for i := range tiles {
			if tiles[i].Slug != m.From {
				continue
			}
			// Service and container names are derived from the slug, so the
			// old service has to go or the renamed instance runs beside it.
			envnet.TearDown(ctx, r.Store, r.Applier.Ops.Cluster, &tiles[i])
			tiles[i].Name, tiles[i].Slug = m.To, m.To
			// RenameTile, not UpdateTile: the slug is not in the general
			// update, deliberately, so this used to tear the instance down
			// and then write nothing at all.
			if err := r.Store.RenameTile(ctx, tiles[i].ID, tiles[i].Name, tiles[i].Slug); err != nil {
				return err
			}
			// And back up under the new name: a service cannot be renamed in
			// place, so without this the instance stays down.
			r.Applier.RestartTile(ctx, &tiles[i])
		}
	}
	return nil
}
