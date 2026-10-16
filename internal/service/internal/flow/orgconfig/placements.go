package orgconfig

import (
	"maps"
	"slices"

	"github.com/FyrmForge/stackr/internal/service/internal/planfile"
)

// placing plans the org's pin, ingress and exits (planfile.PlacingPlan, the
// rules the stack file shares). A pin change that moves envs is an impact
// line, or a blocker when a move check fails; a stack that has its own pin is
// not covered, so its envs are not in EnvNeeds or Moves.
func (p *Plan) placing(f *File, live Live) {
	var need planfile.Needs
	for _, k := range slices.Sorted(maps.Keys(live.EnvNeeds)) {
		need.Storage = need.Storage || live.EnvNeeds[k].Storage
	}
	cs, blockers, warns := planfile.PlacingPlan(planfile.PlacingIn{
		Scope: "org",
		Want:  f.Placing(),
		Have:  live.Current,
		Need:  need,
		Facts: live.Placements,
		Moves: live.Moves,
	})
	p.Changes = append(p.Changes, cs...)
	for _, b := range blockers {
		p.block("%s", b)
	}
	p.Notes = append(p.Notes, warns...)
}
