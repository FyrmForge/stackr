package service

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/planfile"
)

// ApproveOpts is what an approver adds to a plan's approve, for the org
// plan and the server plan alike. Ticked is the removal keys (the plan's
// Optional rows) to apply; unticked ones stay live. Confirm says they saw the
// impact lines of a risky plan. The service enforces both; a surface that
// skips its own prompt is still refused here.
//
// ApproverID is who approved, set by the surface from its session (never read
// from a body). The server apply makes the orgs the file creates and gives
// them this user as owner; an apply with none refuses that step.
type ApproveOpts struct {
	Ticked  []string `json:"ticked"`
	Confirm bool     `json:"confirm"`
	// Keep is the explicit "keep it and close the plan" of a plan whose rows
	// are all removals with none ticked; without it that approve is refused
	// with ErrNothingWillChange.
	Keep       bool   `json:"keep"`
	ApproverID string `json:"-"`
}

// checkApprove is the approve contract over a stored plan's JSON (either
// table: both flow plans embed planfile.Plan, flat in JSON): a blocked plan
// is refused with its blockers, one that only has gaps has nothing to apply,
// a risky one needs Confirm, a tick must name a
// removal row of the plan.
func checkApprove(id, planJSON string, opts ApproveOpts) error {
	var p planfile.Plan
	if err := json.Unmarshal([]byte(planJSON), &p); err != nil {
		return fmt.Errorf("plan %s: %w", id, err)
	}
	if p.Blocked() {
		return errs.Conflictf("This plan is blocked: %s", strings.Join(p.Blockers, "; "))
	}
	if len(p.Changes) == 0 && len(p.Gaps) > 0 {
		return errs.Conflictf("Nothing to apply yet: fill in what the plan is missing.")
	}
	if err := p.Approvable(opts.Ticked, opts.Confirm); err != nil {
		return err
	}
	if !opts.Keep && len(p.Changes) > 0 && len(p.Removals()) == len(p.Changes) && len(p.TickedRemovals(opts.Ticked)) == 0 {
		return ErrNothingWillChange
	}
	return nil
}

// ErrNothingWillChange is the refusal of an approve that would apply
// nothing: every row is a removal and none is ticked. A surface asks "keep it
// and close the plan?" and retries with Keep, or ticks a row.
var ErrNothingWillChange error = errs.Conflict{Msg: "Nothing will change. Tick the removal to apply it."}

// appliedPlanSummary is an applied plan's list line: what was done, not the
// "to review" line it was planned with. Removal rows count as removed when
// ticked, else kept. Same wording as components.AppliedSummary.
// ponytail: duplicates that func (service cannot import ui); one source if a third caller appears.
func appliedPlanSummary(planJSON string, ticked []string) (string, bool) {
	var p planfile.Plan
	if json.Unmarshal([]byte(planJSON), &p) != nil {
		return "", false
	}
	var applied, removed, kept int
	for _, c := range p.Changes {
		switch {
		case !c.Optional:
			applied++
		case slices.Contains(ticked, c.Key):
			removed++
		default:
			kept++
		}
	}
	var parts []string
	if applied > 0 {
		parts = append(parts, fmt.Sprintf("%d applied", applied))
	}
	if removed > 0 {
		parts = append(parts, fmt.Sprintf("%d removed", removed))
	}
	if applied == 0 && removed == 0 {
		parts = append(parts, "nothing changed")
	}
	if kept > 0 {
		parts = append(parts, fmt.Sprintf("%d kept", kept))
	}
	return strings.Join(parts, ", "), true
}
