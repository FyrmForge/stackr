package canvas

import (
	"net/http"
	"slices"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/ui/dialog"
	ui "github.com/FyrmForge/stackr/internal/ui/graph"
)

// creates are the level's create dialogs the viewer may open, by kind of
// level: the route under <page>/-/new-<kind> and its verb.
var creates = map[string][]struct {
	kind  string
	label string
	verb  authz.Verb
}{
	service.CanvasHome:  {{"org", "+ org", "org.create"}},
	service.CanvasOrg:   {{"stack", "+ stack", "stack.create"}, {"connector", "+ connector", "connector.write"}},
	service.CanvasStack: {{"env", "+ env", "env.write"}},
}

func createButtons(c echo.Context, l level) []ui.Create {
	p := middleware.Principal(c)
	var out []ui.Create
	for _, cr := range creates[l.scope.Kind] {
		var r authz.Resource
		if s := middleware.ScopeOf(c); s.Org != nil {
			r.OrgID = s.Org.ID
		}
		if p != nil && authz.Can(p.Access, cr.verb, r) == nil {
			out = append(out, ui.Create{Label: cr.label, URL: l.base + "/-/new-" + cr.kind})
		}
	}
	return out
}

func levelForm(c echo.Context, kind string) dialog.CreateLevelView {
	return dialog.CreateLevelView{Kind: kind, Action: where(c).base + "/-/new-" + kind,
		Name: c.FormValue("name"), From: c.FormValue("from"), Branch: c.FormValue("branch")}
}

// GET <page>/-/new-<org|stack|env>: the empty form into the drawer.
func (h *handler) newLevel(kind string) echo.HandlerFunc {
	return func(c echo.Context) error {
		return respond.HTML(c, http.StatusOK, dialog.CreateLevel(levelForm(c, kind)))
	}
}

// POST <page>/-/new-<kind>: create, then go to the new card's page; a
// refusal re-renders the form with the field marked (422).
func (h *handler) createLevel(kind string) echo.HandlerFunc {
	return func(c echo.Context) error {
		v, s, ctx := levelForm(c, kind), middleware.ScopeOf(c), c.Request().Context()
		var url string
		var err error
		switch kind {
		case "org":
			var og service.Org
			if og, err = h.orch.CreateOrg(ctx, middleware.Principal(c).User.ID); err == nil {
				if og, err = h.orch.RenameOrg(ctx, og.ID, v.Name); err == nil {
					og, err = h.orch.FinishOrg(ctx, og.ID)
				}
			}
			url = "/" + og.Slug
		case "stack":
			var st service.Stack
			st, err = h.orch.CreateStack(ctx, s.Org.ID, v.Name, "")
			url = "/" + s.Org.Slug + "/" + st.Slug
		case "env":
			var e service.Environment
			e, err = h.orch.CreateEnv(ctx, s.Stack.ID, v.Name, service.EnvSpec{Type: "static", FromKind: v.From, FromBranch: v.Branch})
			url = "/" + s.Org.Slug + "/" + s.Stack.Slug + "/" + e.Slug
		}
		if err != nil {
			msg, ok := refused(err)
			if !ok {
				return middleware.HTTPError(err)
			}
			field := "general"
			if inv, ok := errs.IsInvalid(err); ok && slices.Contains([]string{"name", "branch"}, inv.Field) {
				field, msg = inv.Field, inv.Msg
			}
			v.Errors = map[string]string{field: msg}
			return respond.HTML(c, http.StatusUnprocessableEntity, dialog.CreateLevel(v))
		}
		_, err = redirect(c, url)
		return err
	}
}

// GET /:org/-/new-connector, then POST it: the pending connector, and the
// form that hands its manifest to GitHub.
// ponytail: GitHub's callback lands on the page session D builds.
func (h *handler) newConnector(c echo.Context) error {
	return respond.HTML(c, http.StatusOK, dialog.InstallConnector(dialog.InstallConnectorView{Begin: where(c).base + "/-/new-connector"}))
}

func (h *handler) beginConnector(c echo.Context) error {
	v := dialog.InstallConnectorView{Begin: where(c).base + "/-/new-connector"}
	_, action, manifest, err := h.orch.BeginConnector(c.Request().Context(), middleware.ScopeOf(c).Org.ID, c.FormValue("github_org"))
	if err != nil {
		msg, ok := refused(err)
		if !ok {
			return middleware.HTTPError(err)
		}
		v.Error = msg
		return respond.HTML(c, http.StatusUnprocessableEntity, dialog.InstallConnector(v))
	}
	v.Action, v.Manifest = action, manifest
	return respond.HTML(c, http.StatusOK, dialog.InstallConnector(v))
}

// Mount registers the drawers and create dialogs of the home, org and
// stack canvases, and the vars editor of every level.
func (h *handler) Mount(site *echo.Group, a *middleware.Access) {
	h.mountOrg(site, a)
	h.mountStack(site, a)
	h.mountEnv(site, a)
	h.mountConnector(site, a)
	for _, l := range []string{"/:org", "/:org/:stack", "/:org/:stack/:env"} {
		site.GET(l+"/-/vars", h.drawerRoute("vars"), a.Require("org.read"))
		site.POST(l+"/-/vars", h.SetVars, a.Require("variable.write"))
		site.POST(l+"/-/vars/delete", h.DeleteVar, a.Require("variable.write"))
	}
	for _, cr := range []struct {
		page, kind string
		verb       authz.Verb
	}{{"", "org", "org.create"}, {"/:org", "stack", "stack.create"}, {"/:org/:stack", "env", "env.write"}} {
		site.GET(cr.page+"/-/new-"+cr.kind, h.newLevel(cr.kind), a.Require(cr.verb))
		site.POST(cr.page+"/-/new-"+cr.kind, h.createLevel(cr.kind), a.Require(cr.verb))
	}
	site.GET("/:org/-/new-connector", h.newConnector, a.Require("connector.write"))
	site.POST("/:org/-/new-connector", h.beginConnector, a.Require("connector.write"))
}
