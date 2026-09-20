package org

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/components"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/githubapp"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// The onboarding wizard's server side. Step 1 creates the org (POST /orgs does
// the work); steps 2 to 5 post to their own /setup/... route, which is served
// by the same handler as the settings tab that owns the change. The route the
// request arrived on is what decides where it goes back to (backTo), so no
// handler here branches on "am I in the wizard" and no form carries a hidden
// return field that a caller could bend.

// The wizard has two branches, chosen on step 1 and stored in orgs.setup_mode:
// a config-managed org is named and given its domains by its file, so it is
// asked for neither. The step numbers are derived from the branch rather than
// written into each page, so a branch is one line in setupFlow.

// setupFlow is the steps this org's branch walks after the branch question.
func setupFlow(o *repo.Org) []string {
	if o.SetupConfigBranch() {
		return []string{"connector", "config", "team", "done"}
	}
	return []string{"name", "connector", "domain", "team", "done"}
}

// setupTotal is how many dots the stepper draws, the branch question included.
func setupTotal(o *repo.Org) int { return len(setupFlow(o)) + 1 }

// setupStepNo is one step's number on the stepper. The plan screen belongs to
// the config step and shares its number. 0 means the step is not on this
// branch, which only a hand-typed URL reaches.
func setupStepNo(o *repo.Org, step string) int {
	if step == "plan" {
		step = "config"
	}
	for i, s := range setupFlow(o) {
		if s == step {
			return i + 2 // the branch question is step 1
		}
	}
	return 0
}

// setupFirstURL is the step the branch question sends the new draft to.
func setupFirstURL(o *repo.Org) string {
	return "/orgs/" + o.Slug + "/setup/" + setupFlow(o)[0]
}

// setupNextURL is where a step goes once it is done or skipped.
func setupNextURL(o *repo.Org, step string) string {
	flow := setupFlow(o)
	for i, s := range flow {
		if s == step && i+1 < len(flow) {
			return "/orgs/" + o.Slug + "/setup/" + flow[i+1]
		}
	}
	return "/orgs/" + o.Slug + "/setup/done"
}

// setupItem is one line of the step 6 summary.
type setupItem struct {
	Label string
	Href  string
	Done  bool
}

// GET /setup, step 1, the branch question. Also where "+ Create organization"
// points. It writes nothing: a GET that inserts a row means a prefetch or a
// crawler creates an organization. POST /orgs is what makes the draft.
func (h *handler) SetupStart(c echo.Context) error {
	return respond.HTML(c, http.StatusOK, setupBranchPage(c))
}

// GET /orgs/:id/setup/:step, steps 2 to 6. Owner only: every step past the
// first changes org settings, which only an owner may do.
func (h *handler) Setup(c echo.Context) error {
	o, err := h.ownedSettingsOrg(c)
	if err != nil {
		return err
	}
	// The wizard is a first-run flow. Once this org has been through it, its
	// step URLs are stale copies of settings pages, so send the owner to the
	// tab that actually owns the change.
	if o.SetupDoneAt != nil {
		// Said out loud: a tab left open on a step, or an old bookmark, would
		// otherwise land you on a settings page you did not ask for with
		// nothing explaining the jump.
		middleware.SetFlash(c, "Setup is already finished.", middleware.FlashInfo)
		return respond.Redirect(c, setupSettingsTab(o, c.Param("step")))
	}
	step := c.Param("step")
	// A step the other branch owns: the summary lists what this one has, so
	// that is the honest place to land rather than a page that cannot apply.
	if setupStepNo(o, step) == 0 && step != "done" {
		return respond.Redirect(c, "/orgs/"+o.Slug+"/setup/done")
	}
	switch step {
	case "name":
		return respond.HTML(c, http.StatusOK, setupNamePage(c, o))
	case "connector", "config":
		// The step body hangs on GitHub's repo list, so the frame renders at
		// once and the body loads into it (?body=1). No connector means no
		// GitHub call, and that body renders inline.
		conns := h.githubConnectors(c, o)
		var repos []githubapp.Repo
		loaded := len(conns) == 0 || c.QueryParam("body") == "1"
		if len(conns) > 0 && loaded {
			repos = h.connectorRepos(c, conns)
		}
		if c.QueryParam("body") == "1" {
			if step == "connector" {
				return respond.HTML(c, http.StatusOK, setupConnectorBody(c, o, conns, repos))
			}
			return respond.HTML(c, http.StatusOK, setupConfigBody(c, o, conns, repos))
		}
		if step == "connector" {
			return respond.HTML(c, http.StatusOK, setupConnectorPage(c, o, conns, repos, loaded))
		}
		return respond.HTML(c, http.StatusOK, setupConfigPage(c, o, conns, repos, loaded))
	case "domain":
		// A managed org's domains come from its file, so the step shows them
		// rather than asking for one.
		var res []repo.DomainResource
		if o.ConfigManaged() {
			if res, err = h.orgDomains(c, o.ID); err != nil {
				return err
			}
		}
		return respond.HTML(c, http.StatusOK, setupDomainPage(c, o, res, h.setupDomainPrefill(c.Request().Context(), o)))
	case "team":
		ctx := c.Request().Context()
		members, err := h.members.ListMembers(ctx, o.ID)
		if err != nil {
			return err
		}
		// Only owners reach the wizard at all, so the invites (and their
		// tokens) are safe to render here without a second permission check.
		invites, err := h.members.ListInvites(ctx, o.ID)
		if err != nil {
			return err
		}
		return respond.HTML(c, http.StatusOK, setupTeamPage(c, o, peopleRows(members, invites), h.addCandidates(c, o.ID, members), h.mail.Enabled()))
	case "done":
		// Rendering the summary must not close the wizard. It used to, so a
		// glance at step 6 (or a back button) flipped the flag and threw the
		// owner into settings on the next click. Finish is a POST.
		return respond.HTML(c, http.StatusOK, setupDonePage(c, o, h.setupSummary(c, o)))
	}
	return echo.NewHTTPError(http.StatusNotFound, "no such setup step")
}

// SetupDone is POST /orgs/:slug/setup/done, the only thing that closes the
// wizard, and the only writer of orgs.setup_done_at.
func (h *handler) SetupDone(c echo.Context) error {
	o, err := h.ownedSettingsOrg(c)
	if err != nil {
		return err
	}
	// An org still carrying the placeholder has been named by nobody: on the
	// config branch only the apply names it, so finishing here would strand an
	// organization called "Untitled organization" at a random slug, which is
	// the throwaway-name bug this wizard was rebuilt to remove.
	if o.Name == setupDraftName {
		middleware.SetFlash(c, "This organization has no name yet.", middleware.FlashError)
		return respond.Redirect(c, setupFirstURL(o))
	}
	if o.SetupDoneAt == nil {
		now := time.Now().UTC()
		o.SetupDoneAt = &now
		if err := h.store.UpdateOrg(c.Request().Context(), o); err != nil {
			return err
		}
		if err := h.ensureDefaultDomain(c, o); err != nil {
			return err
		}
	}
	return respond.Redirect(c, "/orgs/"+o.Slug)
}

// ensureDefaultDomain gives an org that finished setup with no domain of any
// kind one under the server's own hostname.
//
// Without it such an org silently gets no hostnames at all: EnsureAutoDomain
// (envops.go) returns early when a stack has nothing to nest under, so every
// tile comes up unreachable and nothing on screen says why. Both branches end
// here, the config file that declares no domains:, and the owner who pressed
// "Skip for now" past the form that had this very host prefilled.
//
// Undeclared, so a later apply never deletes it (orgconf.applyDomains only
// removes rows the file adopted), and ranked below a declared one so the file
// wins the moment it names a domain.
func (h *handler) ensureDefaultDomain(c echo.Context, o *repo.Org) error {
	ctx := c.Request().Context()
	host := h.setupDomainPrefill(ctx, o)
	if host == "" {
		return nil // a LAN install has no base domain to build one from
	}
	all, err := h.resources.ListAll(ctx)
	if err != nil {
		return err
	}
	// Anything already visible to this org's stacks is enough, including a
	// server-wide resource every org inherits.
	if len(service.VisibleDomainResources(all, "", o.ID)) > 0 {
		return nil
	}
	return h.store.CreateDomainResource(ctx, &repo.DomainResource{
		ID: uuid.New().String(), Level: "org", OwnerID: o.ID, Host: host,
		CreatedAt: time.Now().UTC(),
	})
}

// inSetup reports whether this request came in on a wizard route.
func inSetup(c echo.Context) bool {
	return strings.HasPrefix(c.Path(), "/orgs/:slug/setup/")
}

// GET /orgs/:slug/setup/config/plan, step 3's second screen. Binding a file
// produces a plan, and the plan is the decision; sending the owner on to step 4
// without showing it means the org they just described never gets built.
func (h *handler) SetupConfigPlan(c echo.Context) error {
	o, err := h.ownedSettingsOrg(c)
	if err != nil {
		return err
	}
	if o.SetupDoneAt != nil {
		return respond.Redirect(c, "/orgs/"+o.Slug+"/plans")
	}
	cp := h.setupPlan(c, o)
	if cp == nil {
		return respond.Redirect(c, "/orgs/"+o.Slug+"/setup/config")
	}
	// The apply landed while this screen was polling: on to step 4, built from
	// the slug the org has now rather than the one the poll arrived on. The
	// redirect reaches htmx as HX-Redirect, so the page navigates itself.
	work := h.orgPlanWork(c, cp)
	if work != nil && work.Done() {
		if cp.Status == "applied" {
			return respond.Redirect(c, setupNextURL(o, "config"))
		}
		// It failed. Stay on the plan, but as a navigation rather than a
		// banner swap, so the screen comes back carrying the error and the
		// Plan again the failed plan needs.
		if c.Request().Header.Get("HX-Request") == "true" {
			return respond.Redirect(c, setupPlanURL(o, cp.ID))
		}
	}
	return respond.HTML(c, http.StatusOK, setupPlanPage(c, o, cp, work))
}

// POST /orgs/:slug/setup/config/plan/:planID/approve
//
// Back to the plan screen, which then advances itself: the apply is queued, so
// there is nothing to report yet, and walking on to step 4 would be walking on
// from an org that is still being built. The screen shows the banner, polls,
// and redirects to the next step when the job lands.
//
// Addressed by org id: the apply this queues can rename the org, and by the
// time the poll comes back the slug this request arrived on is gone.
func (h *handler) SetupApprovePlan(c echo.Context) error {
	o, err := h.approvePlan(c)
	if err != nil {
		return err
	}
	return respond.Redirect(c, setupPlanURL(o, c.Param("planID")))
}

// setupPlanURL is the wizard's plan screen for one named plan. The id in the
// query is what keeps the screen on its own plan: pendingOrgPlans answers with
// the newest plan still waiting, and the moment the apply lands this one stops
// waiting, so a poll without it would find nothing pending and bounce back to
// the binding form instead of moving on.
func setupPlanURL(o *repo.Org, planID string) string {
	return "/orgs/" + o.ID + "/setup/config/plan?plan=" + planID
}

// POST /orgs/:slug/setup/config/plan/:planID/reject
func (h *handler) SetupRejectPlan(c echo.Context) error {
	o, err := h.rejectPlan(c)
	if err != nil {
		return err
	}
	// Back to the form that made it, not on to the next step: the org this
	// branch is describing does not exist until a plan is applied.
	return respond.Redirect(c, "/orgs/"+o.Slug+"/setup/config")
}

// setupPlanCfg is the plan screen's buttons: the same approve and reject as the
// plans page. There is no way past it, this branch's org is named and built by
// the apply, so walking on would leave an org that only exists as a binding.
func setupPlanCfg(o *repo.Org, cp *repo.ConfigPlan, work *repo.WorkItem) components.PlanViewCfg {
	cfg := orgPlanCfg(o, cp, "/orgs/"+o.Slug+"/setup/config/plan/"+cp.ID)
	cfg.ReplanURL = "/orgs/" + o.Slug + "/setup/config"
	cfg.Work, cfg.PollURL = work, setupPlanURL(o, cp.ID)
	return withoutButtonsWhileApplying(cfg, work)
}

// setupPlan is the plan this screen is about: the one named in the query when
// the screen is polling itself, and otherwise whatever is still waiting, which
// is how step 3 is first arrived at.
func (h *handler) setupPlan(c echo.Context, o *repo.Org) *repo.ConfigPlan {
	if id := c.QueryParam("plan"); id != "" {
		cp, err := h.plans.GetOrgPlan(c.Request().Context(), id)
		if err != nil || cp.StackID != o.ID {
			return nil
		}
		return cp
	}
	cp, _ := h.pendingOrgPlans(c, o)
	return cp
}

// setupSummary is the step 6 checklist: what the wizard actually set up. Each
// row links back to its step: settings are locked until Finish, so a settings
// link here would bounce straight back to this page.
func (h *handler) setupSummary(c echo.Context, o *repo.Org) []setupItem {
	ctx := c.Request().Context()
	base := "/orgs/" + o.Slug + "/setup/"
	res, _ := h.orgDomains(c, o.ID)
	members, _ := h.members.ListMembers(ctx, o.ID)
	invites, _ := h.members.ListInvites(ctx, o.ID)
	// A bound config has already produced a plan by the time step 6 renders,
	// and that plan is what someone has to act on, so the summary points at it
	// rather than at the binding form that made it.
	configHref := base + "config"
	if cp, _ := h.pendingOrgPlans(c, o); cp != nil {
		configHref = base + "config/plan"
	}
	items := []setupItem{
		{Label: "Organization created", Done: true},
		{Label: "GitHub connector", Href: base + "connector", Done: len(h.githubConnectors(c, o)) > 0},
	}
	if o.SetupConfigBranch() {
		items = append(items, setupItem{Label: "Config as code", Href: configHref, Done: o.ConfigManaged()})
	} else {
		// The domain row is only honest on the branch that has the step. On the
		// config branch the file declares domains and ensureDefaultDomain runs
		// at Finish, so the row would read "not done" while about to be true.
		items = append(items, setupItem{Label: "Domain", Href: base + "domain", Done: len(res) > 0})
	}
	return append(items, setupItem{Label: "People", Href: base + "team", Done: len(members) > 1 || len(invites) > 0})
}

// setupSettingsTab maps a wizard step onto the settings tab that owns the same
// setting, for orgs that have already finished onboarding.
func setupSettingsTab(o *repo.Org, step string) string {
	base := "/orgs/" + o.Slug + "/settings/"
	switch step {
	case "name":
		return base + "general"
	case "connector":
		return base + "connectors"
	case "config":
		return base + "config"
	case "domain":
		return base + "domains"
	case "team":
		return base + "members"
	}
	return "/orgs/" + o.Slug
}

// connectorRepos is every repository the org's GitHub apps are installed on.
// It doubles as the install check: an app that exists but was never installed
// anywhere lists nothing, which is the state step 2 has to catch (otherwise
// step 3 binds a repo the server cannot read).
func (h *handler) connectorRepos(c echo.Context, conns []repo.Connector) []githubapp.Repo {
	if h.gh == nil {
		return nil
	}
	var out []githubapp.Repo
	// Two apps installed on the same repo list it twice, and the row says
	// nothing about which app it came from: the first one wins.
	seen := map[string]bool{}
	for i := range conns {
		rs, err := h.gh.ListRepos(c.Request().Context(), &conns[i])
		if err != nil {
			c.Logger().Warnf("github repo list (%s): %v", conns[i].Name, err)
			continue
		}
		for _, r := range rs {
			if seen[r.FullName] {
				continue
			}
			seen[r.FullName] = true
			out = append(out, r)
		}
	}
	return out
}

// connectorInstallURL is where GitHub asks which repos an app may see. Empty
// when there is no app to install yet.
func connectorInstallURL(conns []repo.Connector) string {
	for _, cn := range conns {
		if slug := githubapp.ParseConfig(cn.Config).Slug; slug != "" {
			return "https://github.com/apps/" + slug + "/installations/new"
		}
	}
	return ""
}

// backTo is where a mutation lands afterwards. One handler serves both the
// settings tab and the wizard step, and the route it was reached through says
// which, /orgs/:slug/setup/team/members/... goes back to step "team". Reading
// the registered path rather than a form field means the target is a route this
// server registered, so it can never be pointed somewhere else.
func backTo(c echo.Context, o *repo.Org, def string) string {
	rest, ok := strings.CutPrefix(c.Path(), "/orgs/:slug/setup/")
	if !ok {
		return def
	}
	step, _, _ := strings.Cut(rest, "/")
	return "/orgs/" + o.Slug + "/setup/" + step
}

// setupDraftName is what a draft org is called until it is named, by the file
// on the config branch, by the name step on the UI branch. A real name (rather
// than an empty one) keeps the switcher, the active-org pick and delete free of
// a nameless special case; the cost is an abandoned draft showing up in the
// switcher under this. "New organization" is the switcher's own create action
// (components/shell.templ), so the draft does not borrow that wording.
// setupDraftName is service.DraftOrgName under the name the pages already
// use. The value belongs to the service that writes it; the pages only
// compare against it to decide whether an org has been named yet.
const setupDraftName = service.DraftOrgName

// setupNamePrefill leaves the name step empty rather than asking the owner to
// clear a placeholder they never typed.
func setupNamePrefill(o *repo.Org) string {
	if o.Name == setupDraftName {
		return ""
	}
	return o.Name
}

// POST /orgs/:slug/setup/mode, the branch switch, offered both ways so step 1
// is not a one-way door. Switching to the UI branch drops the binding: leaving
// it would keep ConfigManaged() true with no file behind it, and any plan
// waiting on that binding is rejected with it. No confirm: on that branch
// nothing has been applied yet, or the org would have its real name and the
// button would not be on screen.
func (h *handler) SetupMode(c echo.Context) error {
	o, err := h.ownedSettingsOrg(c)
	if err != nil {
		return err
	}
	if o.SetupDoneAt != nil {
		return respond.Redirect(c, "/orgs/"+o.Slug)
	}
	mode := c.FormValue("mode")
	if mode != "ui" && mode != "config" {
		return echo.NewHTTPError(http.StatusBadRequest, "no such setup branch")
	}
	ctx := c.Request().Context()
	if mode == "ui" {
		if cp, _ := h.pendingOrgPlans(c, o); cp != nil && cp.Status == "pending" {
			if err := h.plans.SetOrgPlanStatus(ctx, cp.ID, "rejected"); err != nil {
				return err
			}
		}
		o.ConfigConnectorID, o.ConfigRepo, o.ConfigBranch, o.ConfigPath = "", "", "", ""
	}
	o.SetupMode = mode
	if err := h.store.UpdateOrg(ctx, o); err != nil {
		return err
	}
	return respond.Redirect(c, setupFirstURL(o))
}

// setupDomainPrefill guesses <slug>.<root>: the instance domain resource the
// installer's root domain seeded, else BASE_URL's host. A LAN or test install
// has neither and gets an empty field rather than a guess.
func (h *handler) setupDomainPrefill(ctx context.Context, o *repo.Org) string {
	if all, err := h.resources.ListAll(ctx); err == nil {
		for _, r := range all {
			if r.Level == "instance" {
				return o.Slug + "." + r.Host
			}
		}
	}
	if components.BaseURL == "" {
		return ""
	}
	u, err := url.Parse(components.BaseURL)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	return o.Slug + "." + u.Hostname()
}

func setupDotClass(i, step int) string {
	switch {
	case i == step:
		return "bg-rw-accent text-white"
	case i < step:
		return "bg-rw-inset text-rw-muted"
	}
	return "border border-rw-border text-rw-faint"
}

func stepLabel(i int) string { return strconv.Itoa(i) }

// stepperLabel is what the dots say out loud.
func stepperLabel(step, total int) string {
	return "Step " + strconv.Itoa(step) + " of " + strconv.Itoa(total)
}

func repoCount(n int) string {
	if n == 1 {
		return "1 repository"
	}
	return strconv.Itoa(n) + " repositories"
}

func firstRepos(repos []githubapp.Repo, n int) []githubapp.Repo {
	if len(repos) > n {
		return repos[:n]
	}
	return repos
}

// moreRepos is the count the preview list does not show. Zero means the list
// on screen is the whole list.
func moreRepos(repos []githubapp.Repo, n int) int {
	if len(repos) > n {
		return len(repos) - n
	}
	return 0
}
