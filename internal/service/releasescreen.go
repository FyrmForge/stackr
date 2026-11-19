package service

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/orgplan"
)

// Releases screen (releases view RFC, section 5). Scope keys: "" every org
// the viewer sees, ScopeServer the server file, "org" an org, "org/stack" a
// stack, "org/" + ScopeOrgConfig the org's file.
const (
	ScopeServer    = "server"
	ScopeOrgConfig = "-config"

	// screenMax caps the list; the screen says so.
	// ponytail: no paging; a cursor when an install passes this.
	screenMax = 300

	RowPlan = "plan" // a row of the screen: an org or server config plan
)

// ScreenQuery narrows the screen. State is "" (all), "waiting", "failed" or
// "running"; Source a SourceKey; Q a substring of a commit or message.
type ScreenQuery struct{ Scope, State, Source, Q string }

// ScreenScope is one entry of the scope tree. Org entries (Depth 0) are the
// "All of <org>" scope; Depth 1 are its file and stacks.
type ScreenScope struct {
	Key, Label string
	Depth      int
	Waiting    int
}

// ScreenRow is a WindowRow with where it belongs.
type ScreenRow struct {
	WindowRow
	Scope      string // the stack's name, "org config" or "server config"
	Org        string // slug
	OrgID      string
	StackSlug  string
	LogEnv     string // the env slug the stack's job logs are addressed under
	CanPromote bool   // the viewer may take it into an env
}

// ReleaseScreen is the screen's tree, rows (newest first) and the sources
// the rows carry.
type ReleaseScreen struct {
	Scopes  []ScreenScope
	Rows    []ScreenRow
	Sources []string
	Total   int // rows that matched, before the cap
	Waiting map[string]int
}

// SourceKey is the filter key of a release's CreatedBy.
func SourceKey(by string) string {
	if strings.HasPrefix(by, "user:") {
		return "user"
	}
	if by == "" {
		return "push"
	}
	return by
}

// ReleaseScreen lists the releases, failures and config plans of every scope
// the principal can read, with the tree of scopes and what waits in each.
// A stack's rows are the window verb's with its full history; the screen
// adds the plans and the filters.
// ponytail: runs the window per stack of every org (each scans the job
// rows); a per-scope cache when an install holds enough stacks to feel it.
func (o *Orchestrator) ReleaseScreen(ctx context.Context, p *Principal, q ScreenQuery) (ReleaseScreen, error) {
	var sc ReleaseScreen
	sc.Waiting = map[string]int{}
	var orgs []Org
	var err error
	if p.Access.Admin {
		orgs, err = o.orgs.ListAll(ctx)
	} else {
		orgs, err = o.orgs.ListForUser(ctx, p.User.ID)
	}
	if err != nil {
		return sc, err
	}
	var all []ScreenRow
	add := func(key string, rs ...ScreenRow) {
		for _, r := range rs {
			if r.State == StateWaiting {
				sc.Waiting[key]++
			}
		}
	}
	if p.Access.Admin {
		sc.Scopes = append(sc.Scopes, ScreenScope{Key: ScopeServer, Label: "Server config"})
		ps, err := o.ServerPlans(ctx, 50)
		if err != nil {
			return sc, err
		}
		rows := planRows("server config", "", "", true, planIn(ps, func(p ServerPlan) planFields {
			return planFields{p.ID, p.Commit, p.Summary, p.Status, p.Error, p.Source, p.CreatedAt, p.DecidedAt != nil}
		}))
		add(ScopeServer, rows...)
		all = append(all, rows...)
	}
	for _, og := range orgs {
		if authz.Can(p.Access, "org.read", authz.Resource{OrgID: og.ID}) != nil {
			continue
		}
		start := len(sc.Scopes)
		sc.Scopes = append(sc.Scopes,
			ScreenScope{Key: og.Slug, Label: "All of " + og.Name},
			ScreenScope{Key: og.Slug + "/" + ScopeOrgConfig, Label: "Org config", Depth: 1})
		ps, err := o.OrgPlans(ctx, og.ID, 50)
		if err != nil {
			return sc, err
		}
		rows := planRows("org config", og.Slug, og.ID, authz.Can(p.Access, "orgplan.approve", authz.Resource{OrgID: og.ID}) == nil, planIn(ps, func(p OrgPlan) planFields {
			return planFields{p.ID, p.Commit, p.Summary, p.Status, p.Error, "push", p.CreatedAt, p.DecidedAt != nil}
		}))
		add(og.Slug+"/"+ScopeOrgConfig, rows...)
		add(og.Slug, rows...)
		all = append(all, rows...)
		sts, err := o.stacks.List(ctx, og.ID)
		if err != nil {
			return sc, err
		}
		for _, st := range sts {
			sc.Scopes = append(sc.Scopes, ScreenScope{Key: og.Slug + "/" + st.Slug, Label: st.Name, Depth: 1})
			w, err := o.releaseWindow(ctx, st.ID, "", true)
			if err != nil {
				return sc, err
			}
			can := authz.Can(p.Access, "env.write", authz.Resource{OrgID: og.ID, StackID: st.ID}) == nil
			var rows []ScreenRow
			for _, r := range w.Rows {
				rows = append(rows, ScreenRow{WindowRow: r, Scope: st.Name, Org: og.Slug, OrgID: og.ID, StackSlug: st.Slug, LogEnv: w.LogEnv, CanPromote: can})
			}
			add(og.Slug+"/"+st.Slug, rows...)
			add(og.Slug, rows...)
			all = append(all, rows...)
		}
		for i := start; i < len(sc.Scopes); i++ {
			sc.Scopes[i].Waiting = sc.Waiting[sc.Scopes[i].Key]
		}
	}
	if len(sc.Scopes) > 0 && sc.Scopes[0].Key == ScopeServer {
		sc.Scopes[0].Waiting = sc.Waiting[ScopeServer]
	}
	slices.SortStableFunc(all, func(a, b ScreenRow) int { return b.At.Compare(a.At) })
	srcs := map[string]bool{}
	needle := strings.ToLower(strings.TrimSpace(q.Q))
	for _, r := range all {
		if !inScope(r, q.Scope) {
			continue
		}
		srcs[SourceKey(r.By)] = true
		if !matchState(r, q.State) || q.Source != "" && SourceKey(r.By) != q.Source {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(r.Commit+" "+r.Message+" "+r.Err), needle) {
			continue
		}
		if sc.Total++; len(sc.Rows) < screenMax {
			sc.Rows = append(sc.Rows, r)
		}
	}
	sc.Sources = slices.Sorted(func(yield func(string) bool) {
		for k := range srcs {
			if !yield(k) {
				return
			}
		}
	})
	return sc, nil
}

// inScope says a row belongs under the scope key.
func inScope(r ScreenRow, scope string) bool {
	switch {
	case scope == "":
		return true
	case scope == ScopeServer:
		return r.Org == "" && r.Kind == RowPlan
	case !strings.Contains(scope, "/"):
		return r.Org == scope
	case strings.HasSuffix(scope, "/"+ScopeOrgConfig):
		return r.Org+"/"+ScopeOrgConfig == scope && r.Kind == RowPlan
	}
	return r.Org+"/"+r.StackSlug == scope
}

func matchState(r ScreenRow, state string) bool {
	switch state {
	case "":
		return true
	case "waiting":
		return r.State == StateWaiting
	case "failed":
		return r.State == StateFailed
	case "running":
		return r.State == StateRunning || r.State == StateDeploying
	}
	return false
}

// planFields is what a config plan row needs of either plan type.
type planFields struct {
	ID, Commit, Summary, Status, Err, Source string
	At                                       time.Time
	Decided                                  bool
}

func planIn[T any](ps []T, f func(T) planFields) []planFields {
	out := make([]planFields, len(ps))
	for i, p := range ps {
		out[i] = f(p)
	}
	return out
}

// planRows are plans as rows: a pending one waits (or is being applied once
// decided), an error is a failure a later applied or clean plan fixed, and
// plans that were superseded or found nothing to change say so. ps is newest
// first.
func planRows(scope, org, orgID string, can bool, ps []planFields) []ScreenRow {
	var out []ScreenRow
	cured := false // a newer plan applied or had nothing to do
	for _, p := range ps {
		row := ScreenRow{Scope: scope, Org: org, OrgID: orgID, CanPromote: can,
			WindowRow: WindowRow{Kind: RowPlan, ID: p.ID, Commit: p.Commit, Message: p.Summary, At: p.At, By: SourceKeyOf(p.Source), Err: p.Err}}
		switch p.Status {
		case orgplan.Pending:
			row.State = StateWaiting
			if p.Decided {
				row.State = "applying"
			}
		case orgplan.Error:
			row.State, row.Fixed = StateFailed, cured
			row.Message = "Plan failed: " + p.Err
		case orgplan.Applied, orgplan.Clean:
			row.State, cured = p.Status, true
		default:
			row.State = p.Status // rejected, superseded
		}
		out = append(out, row)
	}
	return out
}

// SourceKeyOf is the filter key of a plan's source word.
func SourceKeyOf(s string) string {
	if s == "" || s == "repo" {
		return "push"
	}
	return s
}
