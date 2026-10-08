package service

import (
	"encoding/json"
	"fmt"
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
	Ticked     []string `json:"ticked"`
	Confirm    bool     `json:"confirm"`
	ApproverID string   `json:"-"`
}

// checkApprove is the approve contract over a stored plan's JSON (either
// table: both flow plans embed planfile.Plan, flat in JSON): a blocked plan
// is refused with its blockers, a risky one needs Confirm, a tick must name a
// removal row of the plan.
func checkApprove(id, planJSON string, opts ApproveOpts) error {
	var p planfile.Plan
	if err := json.Unmarshal([]byte(planJSON), &p); err != nil {
		return fmt.Errorf("plan %s: %w", id, err)
	}
	if p.Blocked() {
		return errs.Conflictf("This plan is blocked: %s", strings.Join(p.Blockers, "; "))
	}
	return p.Approvable(opts.Ticked, opts.Confirm)
}
