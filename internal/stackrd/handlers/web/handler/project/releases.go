package project

import (
	"context"
	"net/http"
	neturl "net/url"
	"strings"

	"github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/config/stackconf"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// The releases page: what is built, what runs where, and what the file wants,
// on one neutral URL per stack. Promoting used to be a button on a config
// plan, which meant a code-only push had to invent a plan row to press.

// dialogueRows is how many commits the promote dialogue lists before the rest
// fold away behind "n more".
const dialogueRows = 5

// releaseRow is one commit on the page: the log row, whether its image exists,
// and the rungs it can go to.
type releaseRow struct {
	Row      logRow
	Built    bool
	Building bool
	Targets  []promoteTarget
}

// promoteTarget is one env's Promote button on one commit.
type promoteTarget struct {
	Env      repo.Environment
	Color    string
	URL      string // the dialogue fragment
	Back     bool   // the env runs a newer commit: this is a rollback
	PlanWait bool   // a pending config plan sits at or before this commit
}

// releaseView is everything the page renders.
type releaseView struct {
	Stack *repo.Stack
	Log   commitLog
	Rows  []releaseRow
	Plans []repo.ConfigPlan
	URL   string // the page itself, for return-to
}

// behindOf is how many commits separate sha from the branch head. The window
// rows know it by position; older rows carry what the connector answered.
func (l commitLog) behindOf(sha string) (int, bool) {
	for i, row := range l.Rows {
		if row.Commit.SHA == sha {
			return i, true
		}
	}
	for _, row := range l.Older {
		if row.Commit.SHA == sha && row.HaveBehind {
			return row.Behind, true
		}
	}
	return 0, false
}

// rowFor is the log row for a commit, wherever in the log it sits: the ladder
// strip needs the connector link for a commit an env runs, which may be older
// than the window.
func (l commitLog) rowFor(sha string) *logRow {
	for _, rows := range [][]logRow{l.Rows, l.Older, l.OffBranch} {
		for i := range rows {
			if rows[i].Commit.SHA == sha {
				return &rows[i]
			}
		}
	}
	return nil
}

// planWaiting is the pending config plan a promote to envSlug would leave
// unapplied, or nil. Only a plan at or before the commit counts: one planned
// on a newer commit is not about this promote. A stack-scoped row (EnvSlug "")
// covers every env, so it counts for this one too; an env bound to its own
// config branch has its own rows and only those.
func planWaiting(log commitLog, plans []repo.ConfigPlan, envSlug, commit string) *repo.ConfigPlan {
	for i := range plans {
		cp := &plans[i]
		if (cp.EnvSlug != "" && cp.EnvSlug != envSlug) || cp.Status != "pending" {
			continue
		}
		planAt, okPlan := log.behindOf(cp.CommitSHA)
		at, okCommit := log.behindOf(commit)
		// Unknown distances mean the plan sits outside the window; it is
		// still waiting, so it is still worth saying.
		if !okPlan || !okCommit || planAt >= at {
			return cp
		}
		return nil
	}
	return nil
}

func (h *handler) releaseView(ctx context.Context, p *repo.Stack) releaseView {
	log := h.commitLog(ctx, p)
	plans, _ := h.store.ListConfigPlans(ctx, p.ID, 10)
	v := releaseView{Stack: p, Log: log, Plans: plans, URL: stackURL(p) + "/releases"}
	// Envs[0] is the rung that builds on push; nothing is promoted to it.
	upper := log.Envs
	if len(upper) > 0 {
		upper = upper[1:]
	}
	for _, row := range log.Rows {
		r := releaseRow{Row: row, Built: log.Built[row.Commit.SHA]}
		for _, ch := range row.Chips {
			if ch.State == "building" || ch.State == "deploying" {
				r.Building = true
			}
		}
		for _, env := range upper {
			if log.Runs[env.ID] == row.Commit.SHA {
				continue // already runs it
			}
			t := promoteTarget{
				Env:   env,
				Color: log.Colors[env.ID],
				URL:   "/projects/" + p.ID + "/envs/" + env.Slug + "/promote?commit=" + row.Commit.SHA,
			}
			_, _, t.Back = promoteSpan(log, log.Runs[env.ID], row.Commit.SHA)
			t.PlanWait = planWaiting(log, plans, env.Slug, row.Commit.SHA) != nil
			r.Targets = append(r.Targets, t)
		}
		v.Rows = append(v.Rows, r)
	}
	return v
}

// GET /:org/:stack/releases
func (h *handler) Releases(c echo.Context) error {
	ctx := c.Request().Context()
	p, err := h.resolveStackSlugs(c)
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, releasesPage(c, h.releaseView(ctx, p)))
}

// promoteDialogue is the fragment behind the Promote button: what goes into
// the env, whether a rung below is being skipped, and, on the second step,
// that a config plan is waiting.
type promoteDialogue struct {
	Stack   *repo.Stack
	Env     repo.Environment
	Color   string
	Commit  string // full sha
	Short   string
	Commits []logRow // newest first, what this promote carries
	Count   int      // how many there are, including the ones not listed
	Back    bool     // rollback wording
	Skipped string   // the rung below that does not run this commit
	Plan    *repo.ConfigPlan
	PlanURL string
	Step    string // "" | plan
	Return  string
	PostURL string
}

// promoteEnv resolves the env in the URL and refuses the ones that are not a
// rung: the default env builds on push, and an ephemeral one has no ladder.
func (h *handler) promoteEnv(ctx context.Context, p *repo.Stack, slug string) (*repo.Environment, error) {
	envs, err := h.store.ListEnvironmentsByStack(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	n := 0
	for i := range envs {
		if envs[i].Type != "static" {
			continue
		}
		if envs[i].Slug == slug {
			if n == 0 {
				return nil, echo.NewHTTPError(http.StatusBadRequest, "the first environment builds on push; it is not promoted to")
			}
			return &envs[i], nil
		}
		n++
	}
	return nil, echo.NewHTTPError(http.StatusNotFound, "environment not found")
}

// GET /projects/:id/envs/:slug/promote?commit=&step=
func (h *handler) PromoteDialogue(c echo.Context) error {
	ctx := c.Request().Context()
	p, err := h.loadStack(c, c.Param("id"))
	if err != nil {
		return err
	}
	h.fillOrg(ctx, p)
	env, err := h.promoteEnv(ctx, p, c.Param("slug"))
	if err != nil {
		return err
	}
	commit := c.QueryParam("commit")
	if commit == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "a commit is required")
	}
	log := h.commitLog(ctx, p)
	d := promoteDialogue{
		Stack: p, Env: *env, Color: log.Colors[env.ID], Commit: commit,
		Short: shortSHA(commit), Step: c.QueryParam("step"),
		Return:  localPath(c.QueryParam("return")),
		PostURL: "/projects/" + p.ID + "/envs/" + env.Slug + "/promote",
	}
	d.Commits, d.Count, d.Back = promoteSpan(log, log.Runs[env.ID], commit)
	d.Skipped = h.skippedRungFor(ctx, p, env.Slug, commit)
	plans, _ := h.store.ListConfigPlans(ctx, p.ID, 10)
	if cp := planWaiting(log, plans, env.Slug, commit); cp != nil {
		d.Plan = cp
		d.PlanURL = stackURL(p) + "/plans/" + cp.ID
	} else {
		// The second step is about a plan; without one there is nothing to
		// say, and the step is in the URL a person can retype.
		d.Step = ""
	}
	return respond.HTML(c, http.StatusOK, promoteModal(c, d))
}

// promoteSpan is what a promote carries: the commits between what the env runs
// and the one picked, newest first, out of the window the log holds. The count
// comes from the distances to the head, so it stays right when the gap runs
// past that window. back is a commit older than the one the env runs, a
// rollback, worded as one.
func promoteSpan(log commitLog, runsSHA, commit string) (rows []logRow, count int, back bool) {
	target, haveTarget := log.behindOf(commit)
	current, haveCurrent := log.behindOf(runsSHA)
	back = haveTarget && haveCurrent && target > current
	from, to := target, current
	if back {
		from, to = current, target
	}
	if !haveTarget {
		from = 0
	}
	if !haveCurrent || to > len(log.Rows) {
		to = len(log.Rows)
	}
	if from < 0 {
		from = 0
	}
	if from < to {
		rows = log.Rows[from:to]
	}
	count = len(rows)
	if haveTarget && haveCurrent {
		gap := current - target
		if gap < 0 {
			gap = -gap
		}
		if gap > count {
			count = gap
		}
	}
	return rows, count, back
}

// skippedRungFor names the rung below envSlug when it does not run the commit
// being promoted. Promoting past it is allowed; the dialogue says so.
func (h *handler) skippedRungFor(ctx context.Context, p *repo.Stack, envSlug, commit string) string {
	envs, _ := h.store.ListEnvironmentsByStack(ctx, p.ID)
	var static []repo.Environment
	for i := range envs {
		if envs[i].Type == "static" {
			static = append(static, envs[i])
		}
	}
	for i := range static {
		if static[i].Slug != envSlug || i == 0 {
			continue
		}
		below := static[i-1]
		runs, _, _, _, _ := h.envDeployments(ctx, below.ID)
		if runs == nil || runs.CommitSHA != commit {
			return below.Name
		}
	}
	return ""
}

// POST /projects/:id/envs/:slug/promote, commit, and plan when the config
// goes with it. One endpoint behind every Promote button on the panel.
func (h *handler) PromoteCommit(c echo.Context) error {
	ctx := c.Request().Context()
	p, err := h.loadStack(c, c.Param("id"))
	if err != nil {
		return err
	}
	h.fillOrg(ctx, p)
	env, err := h.promoteEnv(ctx, p, c.Param("slug"))
	if err != nil {
		return err
	}
	commit := strings.TrimSpace(c.FormValue("commit"))
	if commit == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "a commit is required")
	}
	back := localPath(c.FormValue("return"))
	if back == "" {
		back = stackURL(p) + "/releases"
	}
	// The plan applies first, whole, then the images move. A plan is a config
	// diff of the whole stack, not a rung, so it is not scoped to this env and
	// applying it is the same operation as pressing Apply on the plan page.
	if planID := c.FormValue("plan"); planID != "" {
		cp, gerr := h.store.GetConfigPlan(ctx, planID)
		if gerr != nil || cp == nil || cp.StackID != p.ID {
			return echo.NewHTTPError(http.StatusNotFound, "plan not found")
		}
		// Both halves in one job: the images must not move until the whole
		// config has landed, and a restart between them would otherwise leave
		// the promote orphaned.
		if _, err := h.work.Enqueue(ctx, stackconf.ApplyKind, p.ID, stackconf.ApplyJob{
			StackID: p.ID, PlanID: cp.ID, Force: true,
			PromoteEnv: env.Slug, PromoteCommit: commit,
		}); err != nil {
			middleware.SetFlash(c, "Could not queue the apply: "+err.Error(), middleware.FlashError)
			return respond.Redirect(c, back)
		}
		middleware.SetFlash(c, "Applying, then promoting "+env.Slug+".", middleware.FlashSuccess)
		to := stackURL(p) + "/plans/" + cp.ID
		if back != "" {
			to += "?return=" + neturl.QueryEscape(back)
		}
		return respond.Redirect(c, to)
	}
	if err := h.applier.Promote(ctx, p, env.Slug, commit); err != nil {
		middleware.SetFlash(c, "Promote failed: "+err.Error(), middleware.FlashError)
		return respond.Redirect(c, back)
	}
	middleware.SetFlash(c, "Promoted "+shortSHA(commit)+" to "+env.Slug+".", middleware.FlashSuccess)
	return respond.Redirect(c, back)
}

// ExportConfig downloads the stack file that would produce this stack's live
// state: the way out of panel-first and into config-as-code.
// GET /:org/:stack/settings/config/export
func (h *handler) ExportConfig(c echo.Context) error {
	ctx := c.Request().Context()
	p, err := h.resolveStackSlugs(c)
	if err != nil {
		return err
	}
	state, err := stackconf.Planner{Store: h.store}.Snapshot(ctx, p)
	if err != nil {
		return err
	}
	r := stackconf.StateToResolved(p.Name, state)
	// The ladder order is the store's, which Snapshot does not carry.
	if envs, lerr := h.store.ListEnvironmentsByStack(ctx, p.ID); lerr == nil {
		var order []string
		for i := range envs {
			if envs[i].Type == "static" {
				order = append(order, envs[i].Slug)
			}
		}
		if len(order) > 0 {
			r.EnvOrder = order
		}
	}
	out, err := stackconf.ExportYAML(r)
	if err != nil {
		return err
	}
	c.Response().Header().Set("Content-Disposition", `attachment; filename="stackr-compose.yml"`)
	return c.Blob(http.StatusOK, "application/yaml", out)
}
