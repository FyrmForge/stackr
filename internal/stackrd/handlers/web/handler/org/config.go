package org

import (
	"net/http"
	"strings"
	"time"

	"github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/config/orgconf"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/deploy"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/githubapp"
	"github.com/FyrmForge/stackr/internal/stackrd/store/audit"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Org config-as-code (§6): binding + plans, the stack pattern one level up.

// githubConnectors is the org's usable GitHub apps. A connector is created
// before GitHub answers the manifest, so a flow that never came back leaves a
// row with no app behind; counting it would show step 2 an install button for
// an app that does not exist.
func (h *handler) githubConnectors(c echo.Context, o *repo.Org) []repo.Connector {
	conns, err := h.store.ListConnectorsByOrg(c.Request().Context(), o.ID)
	if err != nil {
		return nil
	}
	var out []repo.Connector
	for _, cn := range conns {
		if cn.Provider == "github" && githubapp.ParseConfig(cn.Config).Connected() {
			out = append(out, cn)
		}
	}
	return out
}

// GET /orgs/:id/settings/config
func (h *handler) SettingsConfig(c echo.Context) error {
	o, err := h.settingsOrg(c)
	if err != nil {
		return err
	}
	plans, _ := h.store.ListOrgConfigPlans(c.Request().Context(), o.ID, 10)
	return respond.HTML(c, http.StatusOK, orgConfigPage(c, o, h.githubConnectors(c, o), plans))
}

// POST /orgs/:id/settings/config, save the binding and plan.
func (h *handler) SaveOrgConfig(c echo.Context) error {
	o, err := h.ownedSettingsOrg(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	if c.FormValue("plan_only") == "" {
		connID := c.FormValue("connector_id")
		if connID == "" {
			o.ConfigConnectorID, o.ConfigRepo, o.ConfigBranch, o.ConfigPath = "", "", "", ""
		} else {
			cn, cerr := h.store.GetConnector(ctx, connID)
			if cerr != nil || cn == nil || cn.OrgID != o.ID {
				return echo.NewHTTPError(http.StatusBadRequest, "connector must belong to this organization")
			}
			repoFull := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(c.FormValue("repo")), "https://github.com/"), ".git")
			if repoFull == "" {
				return echo.NewHTTPError(http.StatusBadRequest, "repository required")
			}
			o.ConfigConnectorID = connID
			o.ConfigRepo = repoFull
			o.ConfigBranch = strings.TrimSpace(c.FormValue("branch"))
			o.ConfigPath = strings.TrimSpace(c.FormValue("path"))
		}
		if err := h.store.UpdateOrg(ctx, o); err != nil {
			return err
		}
	}
	if o.ConfigManaged() && h.orgcfg != nil {
		cp, err := h.orgcfg.Plan(ctx, o, "")
		switch {
		case err == orgconf.ErrNoFile:
			middleware.SetFlash(c, "Binding saved. No config file at that path yet, so there is nothing to plan.", middleware.FlashError)
		case err != nil:
			middleware.SetFlash(c, "Binding saved, but planning failed: "+err.Error(), middleware.FlashError)
		case cp.Status == "clean" && c.FormValue("plan_only") == "":
			middleware.SetFlash(c, "Planned. Nothing to change.", middleware.FlashSuccess)
		default:
			// Straight to the plan: it is the thing that needs a decision, and
			// "Plan again" comes from a plan page, so it goes back to one even
			// when the refreshed plan is clean. In the wizard that is step 3's
			// second screen, the same review under the stepper.
			if cp.Status == "clean" {
				middleware.SetFlash(c, "Planned. Nothing to change.", middleware.FlashSuccess)
			} else {
				middleware.SetFlash(c, "Planned.", middleware.FlashSuccess)
			}
			if inSetup(c) {
				return respond.Redirect(c, "/orgs/"+o.Slug+"/setup/config/plan")
			}
			return respond.Redirect(c, "/orgs/"+o.Slug+"/plans/"+cp.ID)
		}
	} else {
		middleware.SetFlash(c, "Binding saved.", middleware.FlashSuccess)
	}
	return respond.Redirect(c, backTo(c, o, "/orgs/"+o.Slug+"/settings/config"))
}

func (h *handler) loadOrgPlan(c echo.Context) (*repo.Org, *repo.ConfigPlan, error) {
	o, err := h.ownedSettingsOrg(c)
	if err != nil {
		return nil, nil, err
	}
	cp, err := h.store.GetOrgConfigPlan(c.Request().Context(), c.Param("planID"))
	if err != nil || cp == nil || cp.StackID != o.ID {
		return nil, nil, echo.NewHTTPError(http.StatusNotFound, "plan not found")
	}
	return o, cp, nil
}

// POST /orgs/:slug/plans/:planID/approve, applies synchronously.
func (h *handler) ApproveOrgPlan(c echo.Context) error {
	o, applied, err := h.approvePlan(c)
	if err != nil {
		return err
	}
	// o.Slug is the post-apply one: a plan that renames the org has already
	// moved it, and redirecting to the slug in the URL would 404.
	if !applied {
		// The failure is on the plan row now; the page is where to read it.
		return respond.Redirect(c, "/orgs/"+o.Slug+"/plans/"+c.Param("planID"))
	}
	return respond.Redirect(c, "/orgs/"+o.Slug+"/plans")
}

// POST /orgs/:slug/plans/:planID/reject
func (h *handler) RejectOrgPlan(c echo.Context) error {
	o, err := h.rejectPlan(c)
	if err != nil {
		return err
	}
	return respond.Redirect(c, "/orgs/"+o.Slug+"/plans")
}

// SetPlanInput fills in a value the config declares and nobody has set, from
// the plan page itself. Before this the only way out was to leave the plan, set
// the variable in the Variables tab, and plan again, and during onboarding the
// Variables tab is not even reachable yet.
//
// Re-planning is part of it: the value changes the diff, and the plan on screen
// has to be the one being approved. The runner supersedes the previous pending
// plan, so the banner count does not double.
func (h *handler) SetPlanInput(c echo.Context) error {
	ctx := c.Request().Context()
	o, _, err := h.loadOrgPlan(c)
	if err != nil {
		return err
	}
	name := strings.TrimSpace(c.FormValue("name"))
	if !varNameRe.MatchString(name) {
		return echo.NewHTTPError(http.StatusBadRequest, "variable name: letters, digits, _ . - only")
	}
	// An empty value would count as set: the row leaves the plan and the tiles
	// waiting on it are released, only to deploy with a blank credential.
	val := c.FormValue("value")
	if val == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "a value is required")
	}
	now := time.Now().UTC()
	if err := h.store.UpsertVariable(ctx, &repo.Variable{
		OwnerKind: repo.OwnerOrg, OwnerID: o.ID, Name: name,
		Value: val, Secret: true, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		return err
	}
	audit.Record(ctx, h.store, audit.Actor(c), audit.Set, repo.OwnerOrg, o.ID, name)
	deploy.ClearWaitingOrg(ctx, h.store, o.ID, name)
	var np *repo.ConfigPlan
	if h.orgcfg != nil {
		var perr error
		if np, perr = h.orgcfg.Plan(ctx, o, ""); perr != nil {
			// The value is saved either way; what is stale is the diff on screen.
			middleware.SetFlash(c, name+" set, but re-planning failed: "+perr.Error(), middleware.FlashError)
		} else {
			middleware.SetFlash(c, name+" set.", middleware.FlashSuccess)
		}
	}
	if inSetup(c) {
		return respond.Redirect(c, "/orgs/"+o.Slug+"/setup/config/plan")
	}
	if np != nil {
		return respond.Redirect(c, "/orgs/"+o.Slug+"/plans/"+np.ID)
	}
	return respond.Redirect(c, "/orgs/"+o.Slug+"/plans")
}

// approvePlan and rejectPlan are the decision itself, without the redirect,
// the wizard's step 3 makes the same two decisions and then continues to step 4
// instead of landing on the plans list.
func (h *handler) approvePlan(c echo.Context) (o *repo.Org, applied bool, err error) {
	o, cp, err := h.loadOrgPlan(c)
	if err != nil {
		return nil, false, err
	}
	if cp.Status != "pending" {
		return nil, false, echo.NewHTTPError(http.StatusConflict, "plan is "+cp.Status)
	}
	if h.orgcfg == nil {
		return nil, false, echo.NewHTTPError(http.StatusServiceUnavailable, "org config runner unavailable")
	}
	if err := h.orgcfg.Apply(c.Request().Context(), o, cp); err != nil {
		middleware.SetFlash(c, "Apply failed: "+err.Error(), middleware.FlashError)
		return o, false, nil
	}
	middleware.SetFlash(c, "Org plan applied.", middleware.FlashSuccess)
	return o, true, nil
}

func (h *handler) rejectPlan(c echo.Context) (*repo.Org, error) {
	o, cp, err := h.loadOrgPlan(c)
	if err != nil {
		return nil, err
	}
	if cp.Status != "pending" {
		return nil, echo.NewHTTPError(http.StatusConflict, "plan is "+cp.Status)
	}
	if err := h.store.SetOrgConfigPlanStatus(c.Request().Context(), cp.ID, "rejected"); err != nil {
		return nil, err
	}
	middleware.SetFlash(c, "Plan rejected.", middleware.FlashSuccess)
	return o, nil
}

// The repo picker's two lookups. Both are owner-only and scoped to one of this
// org's own connectors: what an installation can see is not public knowledge,
// so neither may be reachable with only an org id.
func (h *handler) pickerConnector(c echo.Context) (*repo.Connector, string, error) {
	o, err := h.ownedSettingsOrg(c)
	if err != nil {
		return nil, "", err
	}
	if h.gh == nil {
		return nil, "", echo.NewHTTPError(http.StatusNotFound, "no github client")
	}
	cn, err := h.store.GetConnector(c.Request().Context(), c.Param("connectorID"))
	if err != nil || cn == nil || cn.OrgID != o.ID || cn.Provider != "github" {
		return nil, "", echo.NewHTTPError(http.StatusNotFound, "connector not found")
	}
	full := c.QueryParam("repo")
	if !strings.Contains(full, "/") {
		return nil, "", echo.NewHTTPError(http.StatusBadRequest, "repo must be owner/name")
	}
	return cn, full, nil
}

// GET /orgs/:id/settings/connectors/:connectorID/branches?repo=owner/name
func (h *handler) ConnectorBranches(c echo.Context) error {
	cn, full, err := h.pickerConnector(c)
	if err != nil {
		return err
	}
	names, err := h.gh.ListBranches(c.Request().Context(), cn, full)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, err.Error())
	}
	return c.JSON(http.StatusOK, names)
}

// GET /orgs/:id/settings/connectors/:connectorID/file?repo=&ref=&path=
// Answers whether the config file is already in the repo, so the form can say
// "this will create it" instead of failing at plan time.
func (h *handler) ConnectorFileExists(c echo.Context) error {
	cn, full, err := h.pickerConnector(c)
	if err != nil {
		return err
	}
	path := c.QueryParam("path")
	if path == "" {
		path = orgconf.DefaultPath
	}
	b, err := h.gh.FileContents(c.Request().Context(), cn, full, c.QueryParam("ref"), path)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]any{"path": path, "exists": b != nil})
}
