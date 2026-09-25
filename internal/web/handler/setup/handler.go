// Package setup is v0's org setup wizard (handler/org/setup.go): the branch
// question at /setup, the steps under /:org/-/setup/, owner only, and
// Pending, which turns an unfinished org's requests into the wizard for its
// owner and the holding page for anyone else.
package setup

import (
	"cmp"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	hamrmw "github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	pages "github.com/FyrmForge/stackr/internal/ui/pages/setup"
	"github.com/FyrmForge/stackr/internal/web/render"
)

type handler struct{ orch *service.Orchestrator }

func NewHandler(orch *service.Orchestrator) *handler { return &handler{orch: orch} }

// Mount registers the wizard. Every step past the branch question is an
// owner's: each one changes the org.
func (h *handler) Mount(site *echo.Group, a *middleware.Access) {
	page := a.LoginFirst()
	site.GET("/setup", h.start, page, a.Authed())
	site.POST("/setup", h.create, a.Require("org.create"))
	s, step := "/:org/-/setup", a.Require("org.setup.step")
	site.GET(s+"/:step", h.step, page, step)
	site.GET(s+"/config/plan", h.plan, page, step)
	site.POST(s+"/name", h.name, step)
	site.POST(s+"/mode", h.mode, step)
	site.POST(s+"/done", h.done, step)
	site.POST(s+"/discard", h.discard, step)
	site.POST(s+"/connector", h.connector, a.Require("org.setup.connector"))
	site.POST(s+"/domain", h.domain, a.Require("org.setup.domain"))
	site.POST(s+"/config", h.bind, a.Require("org.config.bind"))
	approve := a.Require("orgplan.approve")
	site.POST(s+"/config/plan/:plan/approve", h.approve, approve)
	site.POST(s+"/config/plan/:plan/reject", h.reject, approve)
	manage := a.Require("member.manage")
	site.POST(s+"/team/members", h.invite, manage)
	site.POST(s+"/team/members/:user/role", h.role, manage)
	site.POST(s+"/team/members/:user/remove", h.remove, manage)
}

// Pending answers Require's SetupPending (v0 setupPending): the owner goes
// to the summary, the one screen that can finish the org; anyone else gets
// the holding page. A drawer or a stream has nowhere to draw a page, so an
// htmx request navigates to the org, which then holds. Mount after Load.
func Pending(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		err := next(c)
		var sp middleware.SetupPending
		if !errors.As(err, &sp) || c.Response().Committed {
			return err
		}
		if sp.Owner {
			return respond.Redirect(c, "/"+sp.Slug+"/-/setup/done")
		}
		if isHTMX(c) {
			return respond.Redirect(c, "/"+sp.Slug)
		}
		return render.Page(c, http.StatusOK, sp.Name, pages.Holding(sp.Name))
	}
}

func isHTMX(c echo.Context) bool { return c.Request().Header.Get("HX-Request") == "true" }

// GET /setup, the branch question, where "+ Create organization" points.
// It writes nothing: POST /setup makes the draft. Someone who may not
// create an org goes to theirs, or is told how to get into one.
func (h *handler) start(c echo.Context) error {
	p := middleware.Principal(c)
	if authz.Can(p.Access, "org.create", authz.Resource{}) == nil {
		return render.Bare(c, http.StatusOK, "Setup: Set up your organization", pages.Branch())
	}
	orgs, err := h.orch.Orgs(c.Request().Context(), p.User.ID)
	if err != nil {
		return middleware.HTTPError(err)
	}
	if len(orgs) > 0 {
		return respond.Redirect(c, "/"+orgs[0].Slug)
	}
	return render.Page(c, http.StatusOK, "Set up", pages.NoOrg())
}

// POST /setup (mode): the caller's draft, on the branch picked.
func (h *handler) create(c echo.Context) error {
	ctx := c.Request().Context()
	og, err := h.orch.CreateOrg(ctx, middleware.Principal(c).User.ID)
	if err != nil {
		return middleware.HTTPError(err)
	}
	if og, err = h.orch.SetOrgSetupMode(ctx, og.ID, c.FormValue("mode")); err != nil {
		return middleware.HTTPError(err)
	}
	return respond.Redirect(c, first(&og))
}

// GET /:org/-/setup/:step. A finished org's step is a stale copy of a
// drawer tab, so it goes there, said out loud; a step the other branch
// owns lands on the summary.
func (h *handler) step(c echo.Context) error {
	og, st := middleware.ScopeOf(c).Org, c.Param("step")
	if og.SetupDoneAt != nil {
		hamrmw.SetFlash(c, "Setup is already finished.", hamrmw.FlashInfo)
		return respond.Redirect(c, settingsTab(og, st))
	}
	if stepNo(og, st) == 0 {
		return respond.Redirect(c, base(og)+"/done")
	}
	var f pages.Frame
	var body templ.Component
	var err error
	switch st {
	case "name":
		f = frame(og, st, "Name your organization", "It becomes the slug in every URL below it.")
		v := pages.NameView{Frame: f, Base: base(og)}
		if og.Name != service.DraftOrgName {
			v.Value = og.Name
		}
		body = pages.Name(v)
	case "connector", "config":
		return h.github(c, og, st)
	case "domain":
		f, body, err = h.domainPage(c, og)
	case "team":
		f, body, err = h.team(c, og)
	case "done":
		f, body, err = h.summary(c, og)
	}
	if err != nil {
		return middleware.HTTPError(err)
	}
	return render.Bare(c, http.StatusOK, "Setup: "+f.Title, body)
}

// github is the connector and config steps. Their body hangs on GitHub's
// repo list, so the frame renders at once and the body loads into it
// (?body=1); no connector means no GitHub call, and the body is inline.
func (h *handler) github(c echo.Context, og *service.Org, st string) error {
	ctx := c.Request().Context()
	conns, err := h.orch.ConnectedConnectors(ctx, og.ID)
	if err != nil {
		return middleware.HTTPError(err)
	}
	body := c.QueryParam("body") == "1"
	v := pages.GitHubView{
		Base:      base(og),
		Next:      next(og, st),
		CSRF:      render.Shell(c, "").CSRF,
		Connected: len(conns) > 0,
		Loaded:    len(conns) == 0 || body,
		Repo:      strings.TrimPrefix(og.ConfigRepo, "https://github.com/"),
		Branch:    og.ConfigBranch,
		Path:      og.ConfigPath,
	}
	for _, k := range conns {
		v.Connectors = append(v.Connectors, pages.Option{Value: k.ID, Label: k.Name, Picked: k.ID == og.ConfigConnectorID})
	}
	if v.Connected && v.Loaded {
		repos, err := h.orch.OrgRepos(ctx, og.ID)
		if err != nil {
			return middleware.HTTPError(err)
		}
		for _, r := range repos {
			v.Repos = append(v.Repos, pages.Repo{
				FullName:      r.FullName,
				DefaultBranch: r.DefaultBranch,
			})
		}
		if v.InstallURL, err = h.orch.ConnectorInstallURL(ctx, og.ID, conns[0].ID); err != nil {
			return middleware.HTTPError(err)
		}
	}
	if st == "connector" {
		v.Frame = frame(og, st, "Connect GitHub", "A private GitHub App for this organization, so there are no tokens to copy around.")
		if body {
			return respond.HTML(c, http.StatusOK, pages.ConnectorBody(v))
		}
		return render.Bare(c, http.StatusOK, "Setup: "+v.Title, pages.Connector(v))
	}
	v.Frame = frame(og, st, "Config as code", "One file in a repo declares the whole organization. Plans are applied by hand unless Auto apply is on in the org's Config tab.")
	if body {
		return respond.HTML(c, http.StatusOK, pages.ConfigBody(v))
	}
	return render.Bare(c, http.StatusOK, "Setup: "+v.Title, pages.Config(v))
}

// domainPage prefills <slug>.<instance host>, else the base URL's host; a
// LAN install has neither and gets an empty box. A config-managed org is
// shown its file's domains rather than asked for one.
func (h *handler) domainPage(c echo.Context, og *service.Org) (pages.Frame, templ.Component, error) {
	rs, err := h.orch.DomainResources(c.Request().Context(), og.ID)
	if err != nil {
		return pages.Frame{}, nil, err
	}
	v := pages.DomainView{
		Frame:   frame(og, "domain", "Add a domain", "Stacks generate their hostnames under it. Point an A record at this server first."),
		Base:    base(og),
		Next:    next(og, "domain"),
		Managed: og.ConfigRepo != "",
	}
	if v.Managed {
		v.Title, v.Subtitle = "Domains", ""
	}
	for _, r := range rs {
		switch {
		case r.OrgID != nil:
			v.Hosts = append(v.Hosts, r.Host)
		case v.Prefill == "":
			v.Prefill = og.Slug + "." + r.Host
		}
	}
	if u, err := url.Parse(comp.BaseURL); v.Prefill == "" && err == nil && u.Hostname() != "" {
		v.Prefill = og.Slug + "." + u.Hostname()
	}
	return v.Frame, pages.Domain(v), nil
}

// team is the org's members, its open invites and the ones that ran out,
// in one list; a server admin also gets the accounts not in it yet (v0
// addCandidates).
// ponytail: names come from the user list (one read), as the Members tab's.
func (h *handler) team(c echo.Context, og *service.Org) (pages.Frame, templ.Component, error) {
	ctx := c.Request().Context()
	v := pages.TeamView{
		Frame: frame(og, "team", "Add people", "Everyone gets an invitation to accept, whether or not they already have an account."),
		Base:  base(og),
		Next:  next(og, "team"),
		Org:   og.Name,
		Roles: h.orch.Roles(),
	}
	ms, err := h.orch.Members(ctx, og.ID)
	if err != nil {
		return v.Frame, nil, err
	}
	us, err := h.orch.Users(ctx)
	if err != nil {
		return v.Frame, nil, err
	}
	ins, err := h.orch.Invites(ctx, og.ID)
	if err != nil {
		return v.Frame, nil, err
	}
	me := middleware.Principal(c).User
	in := map[string]bool{}
	for _, m := range ms {
		in[m.UserID] = true
		p := pages.Person{
			UserID: m.UserID,
			Role:   m.Role,
			State:  "member",
			Self:   m.UserID == me.ID,
		}
		if i := slices.IndexFunc(us, func(u service.User) bool { return u.ID == m.UserID }); i >= 0 {
			p.Name, p.Email = cmp.Or(us[i].Name, us[i].Email), us[i].Email
		}
		v.People = append(v.People, p)
	}
	now := time.Now()
	for _, i := range ins {
		if i.UsedAt != nil {
			continue
		}
		p := pages.Person{
			Name:    i.Email,
			Email:   i.Email,
			Role:    i.Role,
			State:   "pending",
			Expires: i.ExpiresAt.Local().Format("Jan 2 2006"),
			Link:    comp.AbsoluteURL("/invite/" + i.ID),
		}
		if i.ExpiresAt.Before(now) {
			p.State = "expired"
		}
		v.People = append(v.People, p)
	}
	if me.Admin() {
		for _, u := range us {
			if u.Active && !in[u.ID] {
				v.Candidates = append(v.Candidates, pages.Candidate{
					Name:  cmp.Or(u.Name, u.Email),
					Email: u.Email,
				})
			}
		}
	}
	return v.Frame, pages.Team(v), nil
}

// summary is the done step: what the wizard set up, each row linking back
// to its step, since the drawers stay closed until Finish.
func (h *handler) summary(c echo.Context, og *service.Org) (pages.Frame, templ.Component, error) {
	ctx := c.Request().Context()
	b := base(og) + "/"
	v := pages.DoneView{
		Frame:    frame(og, "done", doneHeading(og), doneSubtitle(og)),
		Base:     base(og),
		Name:     og.Name,
		Draft:    og.Name == service.DraftOrgName,
		Config:   og.SetupMode == service.SetupConfig,
		Finished: og.SetupDoneAt != nil,
	}
	conns, err := h.orch.ConnectedConnectors(ctx, og.ID)
	if err != nil {
		return v.Frame, nil, err
	}
	v.Rows = []pages.Row{
		{Label: "Organization created", Done: true},
		{Label: "GitHub connector", Href: b + "connector", Done: len(conns) > 0},
	}
	if v.Config {
		// a waiting plan is what someone has to act on
		href := b + "config"
		ps, err := h.orch.OrgPlans(ctx, og.ID, 1)
		if err != nil {
			return v.Frame, nil, err
		}
		if len(ps) > 0 && ps[0].Status == "pending" {
			href = b + "config/plan"
		}
		v.Rows = append(v.Rows, pages.Row{Label: "Config as code", Href: href, Done: og.ConfigRepo != ""})
	} else {
		rs, err := h.orch.DomainResources(ctx, og.ID)
		if err != nil {
			return v.Frame, nil, err
		}
		own := slices.ContainsFunc(rs, func(r service.DomainResource) bool { return r.OrgID != nil })
		v.Rows = append(v.Rows, pages.Row{Label: "Domain", Href: b + "domain", Done: own})
	}
	ms, err := h.orch.Members(ctx, og.ID)
	if err != nil {
		return v.Frame, nil, err
	}
	ins, err := h.orch.PendingInvites(ctx, og.ID)
	if err != nil {
		return v.Frame, nil, err
	}
	v.Rows = append(v.Rows, pages.Row{Label: "People", Href: b + "team", Done: len(ms) > 1 || len(ins) > 0})
	return v.Frame, pages.Done(v), nil
}

// doneHeading: "Finish setting up Untitled organization" over a body that
// says it has no name yet would be the page arguing with itself.
func doneHeading(og *service.Org) string {
	if og.SetupDoneAt != nil {
		return og.Name + " is ready"
	}
	if og.Name == service.DraftOrgName {
		return "Finish setting up this organization"
	}
	return "Finish setting up " + og.Name
}

func doneSubtitle(og *service.Org) string {
	if og.SetupDoneAt != nil {
		return "Anything skipped is one tab away in settings."
	}
	return "Nothing else in this organization opens until you do."
}

// GET /:org/-/setup/config/plan?plan=&job=, the config step's second
// screen. The plan in the query keeps the screen on its own plan while its
// apply runs; without one it is the latest plan still waiting. The org is
// addressed by id there: the apply renames it under the wait. While the
// apply runs the banner polls this URL every 2s (v0's PlanBody).
func (h *handler) plan(c echo.Context) error {
	og, ctx := middleware.ScopeOf(c).Org, c.Request().Context()
	if og.SetupDoneAt != nil {
		return respond.Redirect(c, settingsTab(og, "config"))
	}
	pl, ok, err := h.pickPlan(c, og)
	if err != nil {
		return middleware.HTTPError(err)
	}
	if !ok || pl.Status == "rejected" || pl.Status == "superseded" {
		return respond.Redirect(c, base(og)+"/config")
	}
	// The apply landed: on to the next step, from the slug the org has now.
	// The poll gets it as HX-Redirect, so the page navigates itself.
	if pl.Status == "applied" {
		return respond.Redirect(c, next(og, "config"))
	}
	job := c.QueryParam("job")
	applying := pl.Status == "pending" && pl.DecidedAt != nil
	// It ended without applying: stay on the plan, as a navigation rather
	// than a banner swap, so the screen comes back carrying why.
	if isHTMX(c) && pl.DecidedAt != nil && !applying {
		return respond.Redirect(c, planURL(og, pl.ID, job))
	}
	v := pages.PlanView{
		Frame:   frame(og, "config", "Review the plan", "This is what applying the config file does to this organization."),
		Summary: pl.Summary,
		Commit:  pl.Commit[:min(8, len(pl.Commit))],
		When:    pl.CreatedAt.Local().Format("Jan 2 15:04:05"),
		Error:   pl.Error,
		Replan:  base(og) + "/config",
	}
	if applying {
		v.Work, v.Poll = "Applying.", planURL(og, pl.ID, job)
	}
	if job != "" {
		j, err := h.orch.GetJob(ctx, job)
		if err != nil {
			return middleware.HTTPError(err)
		}
		// only this plan's apply speaks on its screen
		if line := workLine(j); line != "" && slices.Contains(j.LockSet, "orgplan:"+pl.ID) {
			v.Work, v.Failed = line, j.State == "failed"
		}
	}
	var p service.OrgConfigPlan
	if pl.Plan != "" {
		if err := json.Unmarshal([]byte(pl.Plan), &p); err != nil {
			return middleware.HTTPError(err)
		}
		v.Parsed = true
		v.Errors = p.Blockers
		v.Warnings = p.Notes
		v.Changes = planRows(p)
	}
	if pl.Status == "pending" && !applying {
		v.Approve = base(og) + "/config/plan/" + pl.ID + "/approve"
		v.Reject = base(og) + "/config/plan/" + pl.ID + "/reject"
		if p.Blocked() {
			v.Blocked = "This plan has errors. Fix them in the config and plan again."
		}
	}
	return render.Bare(c, http.StatusOK, "Setup: "+v.Title, pages.Plan(v))
}

// workLine is what the apply banner says, from the job's own state.
func workLine(j service.Job) string {
	switch j.State {
	case "queued":
		return "Queued, waiting to start."
	case "running":
		return "Applying."
	case "failed":
		return "The apply failed: " + j.Error
	case "cancelled":
		return "The apply was cancelled."
	case "superseded":
		return "A newer apply replaced this one."
	}
	return ""
}

// planRows is the org plan in v0's row shape: the env column says "org",
// or the stack's name on a stack's own rows, and a moved: rename is a slug
// change on the old name. The org file never deletes, so no row is a "-"
// and Destroys stays empty.
func planRows(p service.OrgConfigPlan) []pages.Change {
	var rows []pages.Change
	for _, ch := range p.Changes {
		row := pages.Change{
			Kind:  "update",
			Env:   "org",
			Tile:  ch.Tile,
			Field: ch.Field,
			Old:   ch.Old,
			New:   ch.New,
			Note:  ch.Note,
		}
		switch ch.Kind {
		case "create":
			row.Kind, row.Env, row.Tile = "create", ch.Tile, ""
		case "rebind":
			row.Env, row.Tile = ch.Tile, "config"
		case "rename":
			row.Tile, row.Field = ch.Old, "slug"
		case "domain":
			row.Kind, row.Field = "create", "domain"
		case "param":
			row.Kind = "create"
		case "domain-update":
			row.Tile, row.Field = "domain", ch.Tile
			row.Old, row.New = ch.Field+"="+ch.Old, ch.Field+"="+ch.New
		}
		rows = append(rows, row)
	}
	return rows
}

// pickPlan is the plan named in the query, or the latest while it waits
// or failed; ok false = none.
func (h *handler) pickPlan(c echo.Context, og *service.Org) (service.OrgPlan, bool, error) {
	ctx := c.Request().Context()
	if id := c.QueryParam("plan"); id != "" {
		pl, err := h.orch.OrgPlan(ctx, id)
		if errors.Is(err, errs.ErrNotFound) || (err == nil && pl.OrgID != og.ID) {
			return pl, false, nil
		}
		return pl, err == nil, err
	}
	ps, err := h.orch.OrgPlans(ctx, og.ID, 1)
	if err != nil || len(ps) == 0 {
		return service.OrgPlan{}, false, err
	}
	return ps[0], ps[0].Status == "pending" || ps[0].Status == "error", nil
}

// planURL is the plan screen by org id, and the apply it waits on.
func planURL(og *service.Org, planID, jobID string) string {
	u := "/" + og.ID + "/-/setup/config/plan?plan=" + planID
	if jobID != "" {
		u += "&job=" + jobID
	}
	return u
}

// POST /:org/-/setup/name: the first name the org has had, so the step
// moves on. A refusal comes back as the form with what was typed.
func (h *handler) name(c echo.Context) error {
	og := middleware.ScopeOf(c).Org
	n, err := h.orch.RenameOrg(c.Request().Context(), og.ID, c.FormValue("name"))
	if err == nil {
		return respond.Redirect(c, next(&n, "name"))
	}
	msg, ok := render.Refused(err)
	if !ok {
		return middleware.HTTPError(err)
	}
	if inv, ok := errs.IsInvalid(err); ok {
		msg = inv.Msg
	}
	return respond.HTML(c, http.StatusOK, pages.NameForm(pages.NameView{
		Base:  base(og),
		Value: c.FormValue("name"),
		Error: msg,
	}))
}

// POST /:org/-/setup/mode: the branch switch. To by hand drops the binding
// and the plan waiting on it.
func (h *handler) mode(c echo.Context) error {
	og, err := h.orch.SetOrgSetupMode(c.Request().Context(), middleware.ScopeOf(c).Org.ID, c.FormValue("mode"))
	if err != nil {
		return middleware.HTTPError(err)
	}
	return respond.Redirect(c, first(&og))
}

// POST /:org/-/setup/connector (gh_org): a native form, since the answer
// is the page that posts the app manifest to GitHub. GitHub's callback
// brings the owner back to this step.
func (h *handler) connector(c echo.Context) error {
	og := middleware.ScopeOf(c).Org
	_, action, manifest, err := h.orch.BeginConnector(c.Request().Context(), og.ID, c.FormValue("gh_org"))
	if err != nil {
		return back(c, err, base(og)+"/connector")
	}
	return render.Bare(c, http.StatusOK, "Taking you to GitHub", pages.GitHubRedirect(action, manifest))
}

// POST /:org/-/setup/config: bind the file and plan it (connector_id, repo,
// branch, path), or plan the bound file again (plan_only). The plan is the
// decision, so it goes straight to it.
func (h *handler) bind(c echo.Context) error {
	og, ctx, f := middleware.ScopeOf(c).Org, c.Request().Context(), c.FormValue
	var err error
	if f("plan_only") != "" {
		_, err = h.orch.PlanOrgConfig(ctx, og.ID)
	} else {
		repo := strings.TrimSpace(f("repo"))
		if repo == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "repository required")
		}
		_, err = h.orch.SetOrgConfigRepo(ctx, og.ID, f("connector_id"), repo, f("branch"), f("path"), false)
	}
	if err != nil {
		return back(c, err, base(og)+"/config")
	}
	ps, err := h.orch.OrgPlans(ctx, og.ID, 1)
	if err != nil {
		return middleware.HTTPError(err)
	}
	if len(ps) == 0 {
		return respond.Redirect(c, base(og)+"/config")
	}
	// Plan again comes from the plan screen, so it goes back to one even
	// when the fresh plan is clean; a clean bind stays on the form.
	switch pl := ps[0]; {
	case pl.Status == "error":
		hamrmw.SetFlash(c, "Binding saved, but planning failed: "+pl.Error, hamrmw.FlashError)
		return respond.Redirect(c, base(og)+"/config")
	case pl.Status == "clean" && f("plan_only") == "":
		hamrmw.SetFlash(c, "Planned. Nothing to change.", hamrmw.FlashSuccess)
		return respond.Redirect(c, base(og)+"/config")
	case pl.Status == "clean":
		hamrmw.SetFlash(c, "Planned. Nothing to change.", hamrmw.FlashSuccess)
		return respond.Redirect(c, planURL(og, pl.ID, ""))
	default:
		hamrmw.SetFlash(c, "Planned.", hamrmw.FlashSuccess)
		return respond.Redirect(c, planURL(og, pl.ID, ""))
	}
}

// POST …/config/plan/:plan/approve: back to the plan screen, which waits on
// the apply and moves on when it lands.
func (h *handler) approve(c echo.Context) error {
	og, id := middleware.ScopeOf(c).Org, c.Param("plan")
	j, err := h.orch.ApproveOrgPlan(c.Request().Context(), id)
	if err != nil {
		return back(c, err, planURL(og, id, ""))
	}
	return respond.Redirect(c, planURL(og, id, j.ID))
}

// POST …/config/plan/:plan/reject: back to the form that made it; the org
// this branch describes does not exist until a plan is applied.
func (h *handler) reject(c echo.Context) error {
	og, id := middleware.ScopeOf(c).Org, c.Param("plan")
	if _, err := h.orch.RejectOrgPlan(c.Request().Context(), id); err != nil {
		return back(c, err, planURL(og, id, ""))
	}
	hamrmw.SetFlash(c, "Plan rejected.", hamrmw.FlashSuccess)
	return respond.Redirect(c, base(og)+"/config")
}

// POST /:org/-/setup/domain (host, include_env_on_default): the wizard asks
// for one domain; having it is the step done.
func (h *handler) domain(c echo.Context) error {
	og := middleware.ScopeOf(c).Org
	r, err := h.orch.CreateDomainResource(
		c.Request().Context(),
		"org",
		og.ID,
		c.FormValue("host"),
		c.FormValue("include_env_on_default") != "",
		"",
	)
	if err != nil {
		return back(c, err, base(og)+"/domain")
	}
	hamrmw.SetFlash(c, "Domain "+r.Host+" added. This organization's tiles can now claim auto hostnames under it.", hamrmw.FlashSuccess)
	return respond.Redirect(c, next(og, "domain"))
}

// POST /:org/-/setup/team/members (email, role): an invite, whoever the
// address is. Its link is on the row's Copy invite.
func (h *handler) invite(c echo.Context) error {
	og := middleware.ScopeOf(c).Org
	i, err := h.orch.Invite(
		c.Request().Context(),
		og.ID,
		c.FormValue("email"),
		c.FormValue("role"),
		middleware.Principal(c).User.ID,
	)
	if err != nil {
		return back(c, err, base(og)+"/team")
	}
	hamrmw.SetFlash(c, "Invite created for "+i.Email+". This server sends no mail, so use Copy invite on their row and send the link yourself.", hamrmw.FlashSuccess)
	return respond.Redirect(c, base(og)+"/team")
}

func (h *handler) role(c echo.Context) error {
	og := middleware.ScopeOf(c).Org
	if err := h.orch.SetRole(c.Request().Context(), og.ID, c.Param("user"), c.FormValue("role")); err != nil {
		return back(c, err, base(og)+"/team")
	}
	hamrmw.SetFlash(c, "Role updated.", hamrmw.FlashSuccess)
	return respond.Redirect(c, base(og)+"/team")
}

func (h *handler) remove(c echo.Context) error {
	og := middleware.ScopeOf(c).Org
	if err := h.orch.RemoveMember(c.Request().Context(), og.ID, c.Param("user")); err != nil {
		return back(c, err, base(og)+"/team")
	}
	hamrmw.SetFlash(c, "Member removed.", hamrmw.FlashSuccess)
	return respond.Redirect(c, base(og)+"/team")
}

// POST /:org/-/setup/done, the only thing that closes the wizard. An org
// nobody named yet goes back to the step that names it.
func (h *handler) done(c echo.Context) error {
	og := middleware.ScopeOf(c).Org
	n, err := h.orch.FinishOrg(c.Request().Context(), og.ID)
	if err != nil {
		return back(c, err, first(og))
	}
	return respond.Redirect(c, "/"+n.Slug)
}

// POST /:org/-/setup/discard: a real delete. A draft whose file already
// built stacks is refused, and the refusal lands on the summary.
func (h *handler) discard(c echo.Context) error {
	og := middleware.ScopeOf(c).Org
	if err := h.orch.DeleteOrg(c.Request().Context(), og.ID); err != nil {
		return back(c, err, base(og)+"/done")
	}
	return respond.Redirect(c, "/")
}

// back flashes a refusal the owner can act on and returns to url; anything
// else is the error page.
func back(c echo.Context, err error, url string) error {
	msg, ok := render.Refused(err)
	if !ok {
		return middleware.HTTPError(err)
	}
	if inv, ok := errs.IsInvalid(err); ok {
		msg = inv.Msg
	}
	hamrmw.SetFlash(c, msg, hamrmw.FlashError)
	return respond.Redirect(c, url)
}

// flow is the steps the org's branch walks after the branch question: a
// config-managed org is named and given its domains by its file.
func flow(og *service.Org) []string {
	if og.SetupMode == service.SetupConfig {
		return []string{"connector", "config", "team", "done"}
	}
	return []string{"name", "connector", "domain", "team", "done"}
}

func base(og *service.Org) string { return "/" + og.Slug + "/-/setup" }

// stepNo is a step's dot; the branch question is 1. 0 = not on this branch.
func stepNo(og *service.Org, step string) int {
	i := slices.Index(flow(og), step)
	if i < 0 {
		return 0
	}
	return i + 2
}

func frame(og *service.Org, step, title, subtitle string) pages.Frame {
	return pages.Frame{
		Title:    title,
		Subtitle: subtitle,
		Step:     stepNo(og, step),
		Total:    len(flow(og)) + 1,
	}
}

func first(og *service.Org) string { return base(og) + "/" + flow(og)[0] }

// next is where a step goes once it is done or skipped.
func next(og *service.Org, step string) string {
	f := flow(og)
	if i := slices.Index(f, step); i >= 0 && i+1 < len(f) {
		return base(og) + "/" + f[i+1]
	}
	return base(og) + "/done"
}

// settingsTab is the drawer tab that owns a step's setting, for an org past
// the wizard.
func settingsTab(og *service.Org, step string) string {
	tab := map[string]string{
		"name":   "settings",
		"config": "config",
		"domain": "domains",
		"team":   "members",
	}[step]
	if tab == "" {
		return "/" + og.Slug
	}
	return "/" + og.Slug + "?drawer=org:" + og.ID + "&tab=" + tab
}
