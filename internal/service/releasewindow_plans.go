package service

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/orgplan"
	"github.com/FyrmForge/stackr/internal/service/internal/planfile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// planFacts is the part of an org or server plan the window reads.
type planFacts struct {
	ID, Commit, Summary, Status, Err string
	At                               time.Time
	Decided                          bool // approved: its apply is running
}

// planLine is a plan's summary with what its changes name: "1 to add: stack
// dashy", "1 removal to review: route auth.vulpe.dev". The summary is counts
// only; at most two names, then "and N more".
func planLine(summary, planJSON string) string {
	var p planfile.Plan
	if summary == "" || json.Unmarshal([]byte(planJSON), &p) != nil {
		return summary
	}
	var names []string
	for _, c := range p.Changes {
		if c.Tile == "" {
			continue
		}
		label := strings.TrimSuffix(strings.TrimSuffix(c.Kind, "-delete"), "-create")
		if label == c.Kind {
			label = "" // an update of a tile: the tile is the name
		}
		if n := strings.TrimSpace(label + " " + c.Tile); !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	if len(names) > 2 {
		names = append(names[:2], fmt.Sprintf("and %d more", len(names)-2))
	}
	if len(names) == 0 {
		return summary
	}
	return summary + ": " + strings.Join(names, ", ")
}

// windowPlanRows are the window rows of config plans, newest first as given:
// a waiting plan, the failure of the newest one, an apply under way, then a
// few that ended (applied, rejected). Superseded and clean ones are noise.
func windowPlanRows(ps []planFacts) (rows []WindowRow, waiting, failed int) {
	var older []WindowRow
	for i, p := range ps {
		row := WindowRow{Kind: RowPlan, PlanID: p.ID, Commit: p.Commit, Message: p.Summary, At: p.At, Err: ConfigErrLine(p.Err)}
		switch {
		case p.Status == orgplan.Pending && !p.Decided:
			row.State = StateWaiting
			waiting++
		case p.Status == orgplan.Pending:
			row.State = StateDeploying
		case p.Status == orgplan.Error && i == 0: // a newer plan fixes it, as a push does
			row.State = StateFailed
			failed++
		case p.Status == orgplan.Applied:
			row.State = StateApplied
		case p.Status == orgplan.Rejected:
			row.State = StateRejected
		default:
			continue
		}
		if row.State == StateApplied || row.State == StateRejected {
			older = append(older, row)
			continue
		}
		rows = append(rows, row)
	}
	return append(rows, older[:min(len(older), windowOlder)]...), waiting, failed
}

// rank orders the window's groups: a build, failures, waiting, what runs or
// applies, the rest.
func rank(r WindowRow) int {
	switch {
	case r.Kind == RowBuilding:
		return 0
	case r.State == StateFailed:
		return 1
	case r.State == StateWaiting:
		return 2
	case r.State == StateDeploying || r.State == StateRunning:
		return 3
	}
	return 4
}

// WindowStack is a stack of an org window's rows.
type WindowStack struct {
	Name, Slug string
	Envs       []WindowEnv
}

// OrgWindow is the releases window of an org canvas: the org file's plans
// and, with every stack's open releases, the whole list. Waiting and Failed
// count both; OrgWaiting and OrgFailed the plans alone (the "Org config"
// view of the switch).
type OrgWindow struct {
	ReleaseWindow
	OrgWaiting, OrgFailed int
	Stacks                map[string]WindowStack // by id, for the rows' links
}

// ponytail: runs every stack's window on each 3 second tick of every open
// canvas; one read of the org's stacks if an install holds enough of them
// to feel it.
//
// OrgReleaseWindow reads the org window. plans = the viewer may read the
// org's plans. A stack's rows are what waits, fails, builds or runs there:
// its older releases are the Releases screen's.
func (o *Orchestrator) OrgReleaseWindow(ctx context.Context, orgID string, plans bool) (OrgWindow, error) {
	w := OrgWindow{Stacks: map[string]WindowStack{}}
	og, err := o.orgs.Get(ctx, orgID)
	if err != nil {
		return w, err
	}
	w.Bound = og.ConfigRepo != ""
	var rows []WindowRow
	if plans {
		ps, err := o.OrgPlans(ctx, orgID, 20)
		if err != nil {
			return w, err
		}
		facts := make([]planFacts, len(ps))
		for i, p := range ps {
			facts[i] = planFacts{p.ID, p.Commit, planLine(p.Summary, p.Plan), p.Status, p.Error, p.CreatedAt, p.DecidedAt != nil}
		}
		rows, w.OrgWaiting, w.OrgFailed = windowPlanRows(facts)
	}
	w.Waiting, w.Failed = w.OrgWaiting, w.OrgFailed
	sts, err := o.stacks.List(ctx, orgID)
	if err != nil {
		return w, err
	}
	// the org's jobs and releases once for every stack's window
	// ponytail: rungs, ladders, push failures and the building push are
	// still read per stack.
	all, err := o.jobRows.Recent(ctx, 0)
	if err != nil {
		return w, err
	}
	byStack, err := o.jobsByStack(ctx, all, sts)
	if err != nil {
		return w, err
	}
	var flat []Job
	for _, st := range sts {
		flat = append(flat, byStack[st.ID]...)
	}
	if flat, err = o.annotate(ctx, flat); err != nil {
		return w, err
	}
	rels, err := o.releases.ListByOrg(ctx, orgID)
	if err != nil {
		return w, err
	}
	for _, st := range sts {
		n := len(byStack[st.ID])
		js := flat[:n:n]
		flat = flat[n:]
		rs := rels[st.ID]
		if rs == nil {
			rs = []store.Release{} // held: none, not unread
		}
		sw, err := o.releaseWindowOf(ctx, st.ID, "", false, &js, rs)
		if err != nil {
			return w, err
		}
		w.Stacks[st.ID] = WindowStack{Name: sw.Stack, Slug: sw.Slug, Envs: sw.Envs}
		w.Waiting += sw.Waiting
		w.Failed += sw.Failed
		for _, r := range sw.Rows {
			if r.State != StateSuperseded && r.State != StateUndeployed {
				rows = append(rows, r)
			}
		}
	}
	slices.SortStableFunc(rows, func(a, b WindowRow) int { return rank(a) - rank(b) })
	w.Rows = rows
	return w, nil
}

// ServerReleaseWindow reads the window of the cluster chart: the server
// file's plans.
func (o *Orchestrator) ServerReleaseWindow(ctx context.Context) (ReleaseWindow, error) {
	var w ReleaseWindow
	b, err := o.ServerConfigBinding(ctx)
	if err != nil {
		return w, err
	}
	ps, err := o.ServerPlans(ctx, 20)
	if err != nil {
		return w, err
	}
	w.Bound = strings.TrimSpace(b.Repo) != "" || len(ps) > 0 // a local file has plans, no repo
	facts := make([]planFacts, len(ps))
	for i, p := range ps {
		facts[i] = planFacts{p.ID, p.Commit, planLine(p.Summary, p.Plan), p.Status, p.Error, p.CreatedAt, p.DecidedAt != nil}
	}
	w.Rows, w.Waiting, w.Failed = windowPlanRows(facts)
	return w, nil
}
