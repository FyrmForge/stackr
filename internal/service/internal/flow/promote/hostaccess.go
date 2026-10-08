package promote

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/deploy"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/hostgrant"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// checkHostAccess compares the per tile elevated access lines the plan would
// run with against the stack's approved host_grants row; what the row lacks
// is a blocker Apply turns into errs.NeedsApproval.
func (f *Flow) checkHostAccess(ctx context.Context, p *Plan, w *work) error {
	if w.re == nil || f.D.HostGrants == nil {
		return nil
	}
	var rows []store.Tile
	for _, name := range slices.Sorted(maps.Keys(w.re.Tiles)) {
		rows = append(rows, toRow(name, w.re.Tiles[name], w.st, w.e))
	}
	have, err := f.D.HostGrants.Of(ctx, w.st.ID)
	if err != nil {
		return err
	}
	if m := deploy.HostSet(rows...).Missing(have); len(m) > 0 {
		p.block("%s", hostgrant.Text(m))
	}
	return nil
}

// OnlyHostAccess is a plan whose one blocker is elevated access: promoting it
// queues a job that parks on an admin, so it is not a refusal.
func (p *Plan) OnlyHostAccess() bool {
	return len(p.Blockers) == 1 && strings.HasPrefix(p.Blockers[0], hostgrant.Prefix)
}

// needsApproval is Apply's answer to a plan whose only blocker is host
// access: the job parks on an admin instead of failing. Any other blocker
// beside it is a plain Conflict, since approval would not unblock the plan.
func needsApproval(p *Plan, st store.Stack) error {
	if !p.OnlyHostAccess() {
		return nil
	}
	return errs.NeedsApproval{Stack: st.ID, What: p.Blockers[0]}
}
