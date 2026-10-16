package setup

import (
	"encoding/json"
	"net/http"
	"strings"

	hamrmw "github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	pages "github.com/FyrmForge/stackr/internal/ui/pages/setup"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// The server's first-run wizard under /-/setup/server: an admin's, before
// org setup. ServerPending sends an admin here while server_setup_done_at is
// unset; there is no restore branch (restore is docs only).

// ServerBase is where the wizard lives.
const ServerBase = "/-/setup/server"

// MountServer registers the wizard. Mount the group after Load.
func (h *handler) MountServer(site *echo.Group, a *middleware.Access) {
	page, bind := a.LoginFirst(), a.Require("serverconfig.bind")
	site.GET(ServerBase, h.serverStart, page, bind)
	site.GET(ServerBase+"/ui", h.serverHand, page, bind)
	site.GET(ServerBase+"/config", h.serverFile, page, bind)
	site.GET(ServerBase+"/config/plan", h.serverPlan, page, bind)
	site.POST(ServerBase+"/mode", h.serverMode, bind)
	site.POST(ServerBase+"/domains", h.serverDomains, a.Require("serverdefaults.set"))
	site.POST(ServerBase+"/dest", h.serverDest, a.Require("serverdefaults.set"))
	site.POST(ServerBase+"/bind", h.serverBind, bind)
	approve := a.Require("serverplan.approve")
	site.POST(ServerBase+"/config/plan/:plan/approve", h.serverApprove, approve)
	site.POST(ServerBase+"/config/plan/:plan/reject", h.serverReject, approve)
	site.POST(ServerBase+"/done", h.serverDone, bind)
}

// ServerPending sends an admin's page requests to the wizard until it is
// finished. Anyone else, an htmx or API call, a write and the wizard's own
// paths pass. Mount after Load, before the org gate.
func ServerPending(orch *service.Orchestrator) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			r := c.Request()
			p := middleware.Principal(c)
			if p == nil || !p.Access.Admin || r.Method != http.MethodGet || isHTMX(c) ||
				strings.HasPrefix(r.URL.Path, ServerBase) || strings.HasPrefix(r.URL.Path, "/api/") ||
				strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/logout" {
				return next(c)
			}
			s, err := orch.ServerSetupState(r.Context())
			if err != nil {
				return middleware.HTTPError(err)
			}
			if s.Done {
				return next(c)
			}
			return c.Redirect(http.StatusSeeOther, ServerBase)
		}
	}
}

// serverFrame is a titled shell with no stepper: the branches have two or three screens.
func serverFrame(title, subtitle string) pages.Frame {
	return pages.Frame{Title: title, Subtitle: subtitle}
}

// GET /-/setup/server: the branch question, or the branch already picked.
func (h *handler) serverStart(c echo.Context) error {
	s, err := h.orch.ServerSetupState(c.Request().Context())
	if err != nil {
		return middleware.HTTPError(err)
	}
	if s.Done {
		return respond.Redirect(c, "/")
	}
	if s.Mode != "" && c.QueryParam("pick") == "" {
		return respond.Redirect(c, ServerBase+"/"+s.Mode)
	}
	return render.Bare(c, http.StatusOK, "Setup: This server", pages.ServerBranch(pages.ServerBranchView{Base: ServerBase}))
}

// POST /-/setup/server/mode (mode): the branch.
func (h *handler) serverMode(c echo.Context) error {
	s, err := h.orch.SetServerSetupMode(c.Request().Context(), middleware.Principal(c), c.FormValue("mode"))
	if err != nil {
		return back(c, err, ServerBase+"?pick=1")
	}
	return respond.Redirect(c, ServerBase+"/"+s.Mode)
}

// GET /-/setup/server/ui: the by-hand branch.
func (h *handler) serverHand(c echo.Context) error {
	ctx := c.Request().Context()
	v := pages.ServerHandView{
		Frame:    serverFrame("Set up this server", "Everything here can be changed later under admin."),
		Base:     ServerBase,
		AdminURL: "/-/admin/placements",
	}
	for key, dst := range map[string]*string{"root_domain": &v.Root, "panel_domain": &v.Panel, "acme_email": &v.Email} {
		val, err := h.orch.Setting(ctx, key)
		if err != nil {
			return middleware.HTTPError(err)
		}
		*dst = val
	}
	ds, err := h.orch.GlobalBackupDests(ctx)
	if err != nil {
		return middleware.HTTPError(err)
	}
	for _, d := range ds {
		v.Dests = append(v.Dests, pages.ServerDest{Name: d.Name, Detail: d.Bucket})
	}
	ps, err := h.orch.Placements(ctx)
	if err != nil {
		return middleware.HTTPError(err)
	}
	for _, p := range ps {
		line := p.Name + ": "
		if p.NodeName == "" {
			line += "no node yet"
		} else {
			line += p.NodeName
		}
		v.Placements = append(v.Placements, line)
	}
	return render.Bare(c, http.StatusOK, "Setup: This server", pages.ServerHand(v))
}

// POST /-/setup/server/domains (root_domain, panel_domain, acme_email).
func (h *handler) serverDomains(c echo.Context) error {
	vals := map[string]string{}
	for _, k := range []string{"root_domain", "panel_domain", "acme_email"} {
		vals[k] = strings.TrimSpace(c.FormValue(k))
	}
	if err := h.orch.SetSettings(c.Request().Context(), vals); err != nil {
		return back(c, err, ServerBase+"/ui")
	}
	hamrmw.SetFlash(c, "Domains saved.", hamrmw.FlashSuccess)
	return respond.Redirect(c, ServerBase+"/ui")
}

// POST /-/setup/server/dest: a global backup destination.
func (h *handler) serverDest(c echo.Context) error {
	f := c.FormValue
	_, err := h.orch.CreateBackupDest(c.Request().Context(), nil, service.BackupDestSpec{
		Name: f("dest_name"), Endpoint: f("endpoint"), Region: f("region"),
		Bucket: f("bucket"), AccessKey: f("access_key"), SecretKey: f("secret_key"),
	})
	if err != nil {
		return back(c, err, ServerBase+"/ui")
	}
	hamrmw.SetFlash(c, "Destination added.", hamrmw.FlashSuccess)
	return respond.Redirect(c, ServerBase+"/ui")
}

// GET /-/setup/server/config: the file branch.
func (h *handler) serverFile(c echo.Context) error {
	ctx := c.Request().Context()
	b, err := h.orch.ServerConfigBinding(ctx)
	if err != nil {
		return middleware.HTTPError(err)
	}
	cs, err := h.orch.ServerConnectors(ctx)
	if err != nil {
		return middleware.HTTPError(err)
	}
	v := pages.ServerFileView{
		Frame:  serverFrame("Set up from a file", "Bind the repo that holds stackr-server.yml, then approve its plan."),
		Base:   ServerBase,
		Repo:   strings.TrimPrefix(b.Repo, "https://github.com/"),
		Branch: b.Branch, Path: b.Path, Bound: b.Repo != "",
	}
	for _, k := range cs {
		v.Connectors = append(v.Connectors, pages.Option{Value: k.ID, Label: k.Name, Picked: k.ID == b.ConnectorID})
	}
	if v.Bound {
		ps, err := h.orch.ServerPlans(ctx, 1)
		if err != nil {
			return middleware.HTTPError(err)
		}
		if len(ps) > 0 {
			v.PlanURL = ServerBase + "/config/plan"
		}
	}
	return render.Bare(c, http.StatusOK, "Setup: This server", pages.ServerFile(v))
}

// POST /-/setup/server/bind (connector_id, repo, branch, path): bind and plan.
func (h *handler) serverBind(c echo.Context) error {
	f := c.FormValue
	repo := strings.TrimSpace(f("repo"))
	if repo == "" {
		return back(c, errs.Invalidf("repo", "a repository is required"), ServerBase+"/config")
	}
	if _, err := h.orch.BindServerConfig(c.Request().Context(), f("connector_id"), repo, f("branch"), f("path"), false); err != nil {
		return back(c, err, ServerBase+"/config")
	}
	hamrmw.SetFlash(c, "Planned.", hamrmw.FlashSuccess)
	return respond.Redirect(c, ServerBase+"/config/plan")
}

// GET /-/setup/server/config/plan: the latest plan, on the org wizard's plan screen.
func (h *handler) serverPlan(c echo.Context) error {
	ctx := c.Request().Context()
	ps, err := h.orch.ServerPlans(ctx, 1)
	if err != nil {
		return middleware.HTTPError(err)
	}
	if len(ps) == 0 {
		return respond.Redirect(c, ServerBase+"/config")
	}
	pl := ps[0]
	if pl.Status == "applied" {
		return respond.Redirect(c, ServerBase+"/config")
	}
	v := pages.PlanView{
		Frame:   serverFrame("Review the plan", "This is what applying the server file does."),
		Summary: pl.Summary,
		Commit:  pl.Commit[:min(8, len(pl.Commit))],
		When:    pl.CreatedAt.Local().Format("Jan 2 15:04:05"),
		Error:   pl.Error,
	}
	applying := pl.Status == "pending" && pl.DecidedAt != nil
	if applying {
		v.Work, v.Poll = "Applying.", ServerBase+"/config/plan"
	}
	var p service.ServerConfigPlan
	if pl.Plan != "" {
		if err := json.Unmarshal([]byte(pl.Plan), &p); err != nil {
			return middleware.HTTPError(err)
		}
		v.Parsed, v.Errors, v.Warnings = true, p.Blockers, p.Notes
		// gaps are filled on Admin > Config, past the wizard
		for _, g := range p.Gaps {
			what := "Server secret " + g.Key
			if g.Kind == "node" {
				what = "Node " + g.Key
			}
			v.Warnings = append(v.Warnings, what+" is missing: finish setup, then fill it in on Admin > Config.")
		}
		for _, ch := range p.Changes {
			v.Changes = append(v.Changes, pages.Change{Kind: "update", Env: "server", Tile: ch.Tile, Field: ch.Field, Old: ch.Old, New: ch.New, Note: cmpNote(ch.Impact, ch.Note)})
		}
	}
	if pl.Status == "pending" && !applying {
		if len(p.Changes) > 0 || p.Blocked() { // gaps alone have nothing to approve
			v.Approve = ServerBase + "/config/plan/" + pl.ID + "/approve"
		}
		v.Reject = ServerBase + "/config/plan/" + pl.ID + "/reject"
		if p.Blocked() {
			v.Blocked = "This plan has errors. Fix them in the file and plan again."
		}
	}
	return render.Bare(c, http.StatusOK, "Setup: This server", pages.Plan(v))
}

func cmpNote(impact, note string) string {
	if impact != "" {
		return impact
	}
	return note
}

// POST …/config/plan/:plan/approve
func (h *handler) serverApprove(c echo.Context) error {
	vals, _ := c.FormParams()
	if _, err := h.orch.ApproveServerPlan(c.Request().Context(), c.Param("plan"), service.ApproveOpts{
		Ticked: vals["ticked"], Confirm: c.FormValue("confirm") != "",
	}); err != nil {
		return back(c, err, ServerBase+"/config/plan")
	}
	return respond.Redirect(c, ServerBase+"/config/plan")
}

// POST …/config/plan/:plan/reject
func (h *handler) serverReject(c echo.Context) error {
	if _, err := h.orch.RejectServerPlan(c.Request().Context(), c.Param("plan")); err != nil {
		return back(c, err, ServerBase+"/config/plan")
	}
	hamrmw.SetFlash(c, "Plan rejected.", hamrmw.FlashSuccess)
	return respond.Redirect(c, ServerBase+"/config")
}

// POST /-/setup/server/done: stamp it; org setup follows as today.
func (h *handler) serverDone(c echo.Context) error {
	if err := h.orch.FinishServerSetup(c.Request().Context(), middleware.Principal(c)); err != nil {
		return middleware.HTTPError(err)
	}
	return respond.Redirect(c, "/")
}
