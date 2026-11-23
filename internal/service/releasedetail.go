package service

import (
	"context"
	"slices"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/planfile"
)

// ReleaseDetail is one release as the release modal shows it, read for one
// env: what cut it, the changes its promote would make there, and whether a
// person may promote it from the list.
type ReleaseDetail struct {
	ID     string
	Number int
	Stack  string
	// EnvID, Env and EnvSlug are the env the plan is read for.
	EnvID, Env, EnvSlug string
	By                  string // Release.CreatedBy, raw
	AuthorName          string // a push's commit author
	AuthorLogin         string
	At                  time.Time
	// Commit is the config commit the release holds ("" = none), CommitURL
	// its page on the git host.
	Commit, CommitURL, Message string
	// Cause is the headline: the commit message, else what cut it (Leaf.CauseOf:
	// what an image watch moved, "Rebuild of release 23"); "" = nothing to say.
	Cause string
	// State is the release's row state in the env window ("" = not in it).
	State string
	// Problems are why the release needs the full review, in plan words:
	// blockers, impacts, removals, gaps (the flat union, PlanProblems). Risky
	// is len(Problems) > 0. The grouped fields below are the same facts, kept
	// apart for the modal.
	Problems []string
	Risky    bool
	// Blockers refuse the promote; Impacts are what it does to what runs;
	// Removals are rows it drops; Gaps are values still to set ("db.pass not
	// set", never a value).
	Blockers, Impacts, Removals, Gaps []string
	// Changes are the plan's other rows: Old and New where the plan knows
	// them (a version), else New is "changed", never a param's value.
	Changes []ReleaseChange
	// Waiting: this is the newest waiting release of the env. Promotable is
	// Waiting and not Risky: the only case the modal offers Promote.
	Waiting, Promotable bool
}

// ReleaseChange is one plan row for the modal's "What changed".
type ReleaseChange struct{ Label, Old, New string }

// ReleaseDetail reads releaseID for envID of stackID, planned as by would
// approve it (nil by plans as a viewer).
func (o *Orchestrator) ReleaseDetail(ctx context.Context, by *Principal, stackID, envID, releaseID string) (ReleaseDetail, error) {
	r, err := o.releases.Get(ctx, releaseID)
	if err != nil {
		return ReleaseDetail{}, err
	}
	e, err := o.envs.Get(ctx, envID)
	if err != nil {
		return ReleaseDetail{}, err
	}
	if r.StackID != stackID || e.StackID != stackID {
		return ReleaseDetail{}, errs.ErrNotFound
	}
	st, err := o.stacks.Get(ctx, stackID)
	if err != nil {
		return ReleaseDetail{}, err
	}
	pp, err := o.PlanPromote(ctx, by, envID, releaseID)
	if err != nil {
		return ReleaseDetail{}, err
	}
	d := ReleaseDetail{
		ID: r.ID, Number: r.Number, Stack: st.Name, EnvID: e.ID, Env: e.Name, EnvSlug: e.Slug,
		By: r.CreatedBy, AuthorName: r.AuthorName, AuthorLogin: r.AuthorLogin, At: r.CreatedAt, Commit: r.Commit, Message: r.Message,
	}
	d.Cause = r.Message
	if d.Cause == "" {
		d.Cause = o.releases.CauseOf(ctx, r.ID)
	}
	if base := o.commitBase(ctx, o.stackConnector(ctx, st), st.ConfigRepo); r.Commit != "" && base != "" {
		d.CommitURL = base + r.Commit
	}
	d.fill(pp.Plan)
	d.Problems = PlanProblems(pp.Plan)
	d.Risky = len(d.Problems) > 0
	if w, err := o.ReleaseWindow(ctx, stackID, envID); err == nil {
		if i := slices.IndexFunc(w.Rows, func(row WindowRow) bool { return row.Kind == RowRelease && row.ID == releaseID }); i >= 0 {
			d.State = w.Rows[i].State
		}
		d.Waiting = slices.ContainsFunc(w.Rows, func(row WindowRow) bool {
			return row.Kind == RowRelease && row.ID == releaseID && row.State == StateWaiting && slices.Contains(row.WaitEnvs, envID)
		})
	}
	d.Promotable = d.Waiting && !d.Risky && pp.CanDeploy
	return d, nil
}

// fill groups the plan for the modal; the flat Problems stay PlanProblems.
func (d *ReleaseDetail) fill(p *Plan) {
	if p == nil {
		return
	}
	pf := planfile.Plan{Changes: p.Changes, Blockers: p.Blockers, Gaps: p.Gaps}
	d.Blockers = slices.Clone(pf.Blockers)
	d.Impacts = pf.Impacts()
	for _, c := range pf.Removals() {
		d.Removals = append(d.Removals, c.Line())
	}
	for _, g := range pf.Gaps {
		if g.Kind == "node" {
			d.Gaps = append(d.Gaps, "node "+g.Key+" is not available")
		} else {
			d.Gaps = append(d.Gaps, g.Key+" not set")
		}
	}
	for _, c := range p.Changes {
		switch {
		case c.Optional:
		case c.Kind == "param":
			d.Changes = append(d.Changes, ReleaseChange{Label: "param " + c.Field, New: "changed"})
		case c.Old != "" && c.New != "" && c.Tile != "":
			d.Changes = append(d.Changes, ReleaseChange{Label: c.Tile + " " + c.Field, Old: c.Old, New: c.New})
		case c.Kind == "update" && c.Tile != "" && c.Field != "":
			d.Changes = append(d.Changes, ReleaseChange{Label: c.Tile + " " + c.Field, New: "changed"})
		default:
			d.Changes = append(d.Changes, ReleaseChange{Label: c.Line()})
		}
	}
}

// gapText is a gap in plan words: a value to set (a secret or a param) says
// "not set", a node the plan wants says so.
func gapText(g planfile.Gap) string {
	if g.Kind == "node" {
		return "Node " + g.Key + " is not available"
	}
	return "Value " + g.Key + " is not set"
}

// PlanProblems are the reasons a promote plan is not safe to apply from a
// list, in plan words: blockers, then impact lines, removals and gaps. Empty
// = the plan is clean. It holds whenever planfile.Plan.AutoOK is false (a
// blocker, impact or removal is listed; an empty plan with gaps lists them),
// and it also counts a gap beside real changes, which AutoOK lets through but
// a promote from a list must not: the value it needs is not set.
func PlanProblems(p *Plan) []string {
	if p == nil {
		return nil
	}
	pf := planfile.Plan{Changes: p.Changes, Blockers: p.Blockers, Gaps: p.Gaps}
	out := slices.Clone(pf.Blockers)
	out = append(out, pf.Impacts()...)
	for _, c := range pf.Removals() {
		out = append(out, "Removes "+c.Line())
	}
	for _, g := range pf.Gaps {
		out = append(out, gapText(g))
	}
	return out
}
