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
	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
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
	env, err := h.releases.Target(ctx, p, slug)
	if err != nil {
		return nil, stackrmw.HTTP(err)
	}
	return env, nil
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
	back := localPath(c.FormValue("return"))
	if back == "" {
		back = stackURL(p) + "/releases"
	}
	planID := c.FormValue("plan")
	// The plan applies first, whole, then the images move — one job, so a
	// restart between the halves cannot leave the promote orphaned. Both the
	// dedupe key and the enqueueing are the service's; this used to build the
	// job by hand under the stack id and lose the plan-id dedupe.
	//
	// Force is the form's checkbox now. It was hard-coded true here, so the
	// panel's button silently overrode a per-environment apply policy that the
	// API and the CLI respect.
	if _, err := h.releases.Promote(ctx, p, env, service.PromoteReq{
		Commit: commit, PlanID: planID, Force: c.FormValue("force") != "",
	}); err != nil {
		middleware.SetFlash(c, "Promote failed: "+err.Error(), middleware.FlashError)
		return respond.Redirect(c, back)
	}
	if planID != "" {
		middleware.SetFlash(c, "Applying, then promoting "+env.Slug+".", middleware.FlashSuccess)
		return respond.Redirect(c, stackURL(p)+"/plans/"+planID+"?return="+neturl.QueryEscape(back))
	}
	middleware.SetFlash(c, "Promoting "+shortSHA(commit)+" to "+env.Slug+".", middleware.FlashSuccess)
	return respond.Redirect(c, back)
}

// ExportConfig downloads the stack file that would produce this stack's live
// state: the way out of panel-first and into config-as-code.
// GET /:org/:stack/settings/config/export
func (h *handler) ExportConfig(c echo.Context) error {
	p, err := h.resolveStackSlugs(c)
	if err != nil {
		return err
	}
	out, err := stackconf.ExportStack(c.Request().Context(), h.store, p)
	if err != nil {
		return err
	}
	c.Response().Header().Set("Content-Disposition", `attachment; filename="stackr-compose.yml"`)
	return c.Blob(http.StatusOK, "application/yaml", out)
}
