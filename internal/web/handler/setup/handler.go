// Package setup is v0's org setup wizard (handler/org/setup.go): the branch
// question at /setup, the steps under /:org/-/setup/, owner only, and
// Pending, which turns an unfinished org's requests into the wizard for its
// owner and the holding page for anyone else.
package setup

import (
	"cmp"
	"context"
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
	"github.com/FyrmForge/stackr/internal/ui/connectors"
	pages "github.com/FyrmForge/stackr/internal/ui/pages/setup"
	"github.com/FyrmForge/stackr/internal/web/connpanel"
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
	site.POST(s+"/domain", h.domain, a.Require("org.setup.domain"))
	site.POST(s+"/config", h.bind, a.Require("org.config.bind"))
	site.GET(s+"/config/connector/:connector/branches", h.branches, step)
	site.GET(s+"/config/connector/:connector/files", h.files, step)
	approve := a.Require("orgplan.approve")
	site.POST(s+"/config/plan/:plan/approve", h.approve, approve)
	site.POST(s+"/config/plan/:plan/reject", h.reject, approve)
	manage := a.Require("member.manage")
	site.POST(s+"/team/members", h.invite, manage)
	site.POST(s+"/team/members/:user/role", h.role, manage)
	site.POST(s+"/team/members/:user/remove", h.remove, manage)
	// the connector step's add dialog: the org's git connectors, under the
	// wizard's own path while the rest of the org waits for it
	site.GET(s+"/connectors/new", h.newGitConnector, step)
	h.orgGitPanel().Mount(site, s+"/connectors", step, nil)
}

// orgGitPanel is the org's git connectors on the connector step: a redirect
// setup comes back to the step.
func (h *handler) orgGitPanel() connpanel.Panel {
	p := connpanel.Panel{
		Orch: h.orch,
		Org: func(c echo.Context) *string {
			id := middleware.ScopeOf(c).Org.ID
			return &id
		},
		Base:    func(c echo.Context) string { return base(middleware.ScopeOf(c).Org) + "/connectors" },
		DNSBase: func(c echo.Context) string { return base(middleware.ScopeOf(c).Org) + "/connectors" },
		Refresh: func(c echo.Context) string { return base(middleware.ScopeOf(c).Org) + "/connector" },
		Only:    service.CapGit,
	}
	return p
}

// GET /:org/-/setup/connectors/new: the add dialog, straight on the git
// kind when there is one, else on a picker of them.
func (h *handler) newGitConnector(c echo.Context) error {
	p := h.orgGitPanel()
	if ks := p.KindsWith(service.CapGit); len(ks) == 1 {
		return respond.HTML(c, http.StatusOK, p.Open(c, ks[0]))
	}
	return respond.HTML(c, http.StatusOK, connectors.Modal("Add a connector", "", connectors.Picker(p.Kinds(c, service.CapGit))))
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
func (h *handler) step(c echo.Context) error { return h.showStep(c, c.Param("step")) }

func (h *handler) showStep(c echo.Context, st string) error {
	og := middleware.ScopeOf(c).Org
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
		return h.git(c, og, st)
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
	return render.Bare(c, status(c), "Setup: "+f.Title, body)
}

// git is the connector and config steps. Their body hangs on the
// provider's repo list, so the frame renders at once and the body loads
// into it (?body=1); no connector means no provider call, and the body is
// inline.
func (h *handler) git(c echo.Context, og *service.Org, st string) error {
	ctx := c.Request().Context()
	conns, err := h.orch.ConnectedConnectors(ctx, og.ID)
	if err != nil {
		return middleware.HTTPError(err)
	}
	body := c.QueryParam("body") == "1"
	v := pages.GitView{
		Base:      base(og),
		Next:      next(og, st),
		Label:     h.gitLabel(),
		Add:       base(og) + "/connectors/new",
		Connected: len(conns) > 0,
		Loaded:    len(conns) == 0 || body,
		Repo:      shortRepo(og.ConfigRepo, conns),
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
		// installing a shared server connector is the admin's job: no link
		// for it; the install comes back to this step
		if i := slices.IndexFunc(conns, func(k service.Connector) bool { return k.OrgID != nil }); i >= 0 {
			if v.InstallURL, err = h.orch.ConnectorInstallBack(ctx, &og.ID, conns[i].ID, base(og)+"/"+st); err != nil {
				return middleware.HTTPError(err)
			}
		}
	}
	if st == "connector" {
		sub := "A " + v.Label + " connector for this organization, so there are no tokens to copy around."
		if v.Connected && !slices.ContainsFunc(conns, func(k service.Connector) bool { return k.OrgID != nil }) {
			sub = "A server admin shared a " + v.Label + " connector with this organization, so there are no tokens to copy around."
		}
		v.Frame = frame(og, st, "Connect "+v.Label, sub)
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
	if u, err := url.Parse(h.panelURL(c.Request().Context(), "")); v.Prefill == "" && err == nil && u.Hostname() != "" {
		v.Prefill = og.Slug + "." + u.Hostname()
	}
	return v.Frame, pages.Domain(v), nil
}

// panelURL is path on the panel's public address (comp.PanelURL with the
// panel_domain setting), as the org drawer's invite links are built.
func (h *handler) panelURL(ctx context.Context, path string) string {
	host, _ := h.orch.Setting(ctx, "panel_domain")
	return comp.PanelURL(host, path)
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
			Link:    h.panelURL(ctx, "/invite/"+i.ID),
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
		{Label: h.gitLabel() + " connector", Href: b + "connector", Done: len(conns) > 0},
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
// GET /:org/-/setup/config/connector/:connector/branches?repo=, the repo
// picker's branch list.
func (h *handler) branches(c echo.Context) error {
	og := middleware.ScopeOf(c).Org
	bs, err := h.orch.ConnectorBranches(c.Request().Context(), og.ID, c.Param("connector"), c.QueryParam("repo"))
	if err != nil {
		return middleware.HTTPError(err)
	}
	return respond.JSON(c, http.StatusOK, bs)
}

// GET /:org/-/setup/config/connector/:connector/files?repo=&ref=, the repo
// picker's file list.
func (h *handler) files(c echo.Context) error {
	og := middleware.ScopeOf(c).Org
	fs, err := h.orch.ConnectorYAMLFiles(c.Request().Context(), og.ID, c.Param("connector"), c.QueryParam("repo"), c.QueryParam("ref"))
	if err != nil {
		return middleware.HTTPError(err)
	}
	if fs == nil {
		fs = []string{}
	}
	return respond.JSON(c, http.StatusOK, fs)
}

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
	var j service.Job
	mine := false
	if job != "" {
		if j, err = h.orch.GetJob(ctx, job); err != nil {
			return middleware.HTTPError(err)
		}
		// only this plan's apply speaks on its screen
		mine = slices.Contains(j.LockSet, "orgplan:"+pl.ID)
	}
	// The apply records the plan's end a moment before its job row ends:
	// until the row does, it is still applying, or the screen stops polling
	// on a banner that says "Applying.".
	applying := pl.Status == "pending" && pl.DecidedAt != nil ||
		mine && (j.State == "queued" || j.State == "running")
	// It ended without applying: stay on the plan, as a navigation rather
	// than a banner swap, so the screen comes back carrying why.
	if isHTMX(c) && pl.DecidedAt != nil && !applying {
		return respond.Redirect(c, planURL(og, pl.ID, job))
	}
	v := pages.PlanView{
		Frame:   frame(og, "config", "Review the plan", "This is what applying the config file does to this organization."),
		Summary: pl.Summary,
		Commit:  pl.Commit[:min(8, len(pl.Commit))],
		When:    pl.CreatedAt,
		Error:   pl.Error,
		Replan:  base(og) + "/config",
	}
	if applying {
		v.Work, v.Poll = "Applying.", planURL(og, pl.ID, job)
	}
	if line := workLine(j); line != "" && mine {
		v.Work, v.Failed = line, j.State == "failed"
	}
	var p service.OrgConfigPlan
	if pl.Plan != "" {
		if err := json.Unmarshal([]byte(pl.Plan), &p); err != nil {
			return middleware.HTTPError(err)
		}
		v.Parsed = true
		v.Errors = p.Blockers
		v.Warnings = p.Notes
		// gaps are filled on the org's Config tab, past the wizard
		for _, g := range p.Gaps {
			v.Warnings = append(v.Warnings, "Secret "+g.Key+" ("+g.Env+") has no value yet: its tiles wait until you set it on the org's Config tab.")
		}
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
	case "skipped":
		return "The apply was skipped; its node was offline."
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
			return back(c, errs.Invalidf("repo", "a repository is required"), base(og)+"/config")
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
		// the plan screen shows the file's problems and the lines they are on
		hamrmw.SetFlash(c, "Binding saved, but the file has problems.", hamrmw.FlashError)
		return respond.Redirect(c, planURL(og, pl.ID, ""))
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
	vals, _ := c.FormParams()
	j, err := h.orch.ApproveOrgPlan(c.Request().Context(), id, service.ApproveOpts{
		Ticked:  vals["ticked"],
		Confirm: c.FormValue("confirm") != "",
	})
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
		return refill(c, err, func() error { return h.showStep(c, "domain") })
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
		return refill(c, err, func() error { return h.showStep(c, "team") })
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

// refill answers a refused save with its step again, in both wizards: what
// was posted stays in the form (secrets too: they came from this browser)
// and the message sits under the field it names, else over the form. show
// renders the step; htmx swaps its shell in place of the old one. Anything
// but a refusal is the error page.
func refill(c echo.Context, err error, show func() error) error {
	msg, ok := render.Refused(err)
	if !ok {
		return middleware.HTTPError(err)
	}
	r := pages.Refill{Action: c.Request().URL.Path, Msg: msg}
	if inv, ok := errs.IsInvalid(err); ok {
		r.Field, r.Msg = inv.Field, inv.Msg
	}
	if r.Form, err = c.FormParams(); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad form")
	}
	req := c.Request()
	c.SetRequest(req.WithContext(pages.WithRefill(req.Context(), r)))
	h := c.Response().Header()
	h.Set("HX-Retarget", "#"+pages.ShellID)
	h.Set("HX-Reselect", "#"+pages.ShellID)
	h.Set("HX-Reswap", "outerHTML")
	return show()
}

// status is a step page's: 422 when it answers a refused save.
func status(c echo.Context) int {
	if pages.Refilled(c.Request().Context()) {
		return http.StatusUnprocessableEntity
	}
	return http.StatusOK
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
	// the branch question is not reopened: posting it makes a new draft
	steps := []pages.StepLink{{Name: "Start"}}
	for _, s := range flow(og) {
		steps = append(steps, pages.StepLink{Name: stepNames[s], Href: base(og) + "/" + s})
	}
	return pages.Frame{
		Title:    title,
		Subtitle: subtitle,
		Step:     stepNo(og, step),
		Steps:    steps,
	}
}

// stepNames are the org steps as the stepper names them.
var stepNames = map[string]string{
	"name": "Name", "connector": "Git", "config": "Config", "domain": "Domain", "team": "People", "done": "Summary",
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

// gitLabel is how the steps name the git kind: the one there is, else
// "git".
func (h *handler) gitLabel() string {
	var ls []string
	for _, k := range h.orch.ConnectorKinds() {
		if slices.Contains(k.Caps, service.CapGit) {
			ls = append(ls, k.Label)
		}
	}
	if len(ls) == 1 {
		return ls[0]
	}
	return "git"
}

// shortRepo is repo as the picker lists it, owner/name, when it is on the
// host of one of cs; else as stored.
func shortRepo(repo string, cs []service.Connector) string {
	for _, c := range cs {
		if p := "https://" + c.Host + "/"; strings.HasPrefix(repo, p) {
			return strings.TrimPrefix(repo, p)
		}
	}
	return repo
}
