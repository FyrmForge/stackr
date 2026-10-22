package canvas

import (
	"errors"
	"fmt"
	"net/http"
	"slices"

	hamrmw "github.com/FyrmForge/hamr/pkg/middleware"
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
// level: the route under <page>/-/new-<kind> and its verb. Home's is the
// setup wizard, a page (v0's link to /setup).
var creates = map[string][]struct {
	kind  string
	label string
	verb  authz.Verb
}{
	service.CanvasHome: {{"org", "+ Create organization", "org.create"}},
	service.CanvasOrg: {
		{"stack", "+ Create stack", "stack.create"},
		{"connector", "+ Create connector", "connector.write"},
	},
	service.CanvasStack: {{"env", "+ Create environment", "env.write"}},
	service.CanvasEnv:   {{"tile", "+ Create", "tile.write"}}, // the env package serves it
}

func createButtons(c echo.Context, l level) []ui.Create {
	p := middleware.Principal(c)
	var out []ui.Create
	for _, cr := range creates[l.scope.Kind] {
		var r authz.Resource
		if s := middleware.ScopeOf(c); s.Org != nil {
			r.OrgID = s.Org.ID
		}
		if p == nil || authz.Can(p.Access, cr.verb, r) != nil {
			continue
		}
		if cr.kind == "org" {
			out = append(out, ui.Create{Label: cr.label, URL: "/setup", Nav: true})
			continue
		}
		out = append(out, ui.Create{Label: cr.label, URL: l.base + "/-/new-" + cr.kind})
	}
	return out
}

func (h *handler) levelForm(c echo.Context, kind string) dialog.CreateLevelView {
	v := dialog.CreateLevelView{
		Kind:   kind,
		Action: where(c).base + "/-/new-" + kind,
		Name:   c.FormValue("name"),
		From:   c.FormValue("from"),
		Branch: c.FormValue("branch"),
	}
	if s := middleware.ScopeOf(c); kind == "env" && s.Stack != nil {
		// the bottom rung has nothing to promote from: offer only a branch
		if l, err := h.orch.Ladder(c.Request().Context(), s.Stack.ID); err == nil && len(l) == 0 {
			v.First = true
		}
		if v.Branch == "" {
			v.Branch, _ = h.orch.EnvDefaultBranch(c.Request().Context(), s.Stack.ID)
		}
	}
	return v
}

// GET <page>/-/new-<stack|env>: the empty form into the drawer.
func (h *handler) newLevel(kind string) echo.HandlerFunc {
	return func(c echo.Context) error {
		return respond.HTML(c, http.StatusOK, dialog.CreateLevel(h.levelForm(c, kind)))
	}
}

// POST <page>/-/new-<kind>: create, then go to the new card's page; a
// refusal re-renders the form with the field marked (422).
func (h *handler) createLevel(kind string) echo.HandlerFunc {
	return func(c echo.Context) error {
		v, s, ctx := h.levelForm(c, kind), middleware.ScopeOf(c), c.Request().Context()
		var url string
		var err error
		switch kind {
		case "stack":
			var st service.Stack
			st, err = h.orch.CreateStack(ctx, s.Org.ID, v.Name, "")
			url = "/" + s.Org.Slug + "/" + st.Slug
		case "env":
			var e service.Environment
			if err = h.orch.JoinsLockedTier(ctx, middleware.Principal(c), s.Stack.ID, v.Name, ""); err == nil {
				e, err = h.orch.CreateEnv(
					ctx,
					s.Stack.ID,
					v.Name,
					service.EnvSpec{Type: "static", FromKind: v.From, FromBranch: v.Branch},
				)
			}
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
func (h *handler) newConnector(c echo.Context) error {
	return respond.HTML(
		c,
		http.StatusOK,
		dialog.InstallConnector(dialog.InstallConnectorView{Begin: where(c).base + "/-/new-connector"}),
	)
}

func (h *handler) beginConnector(c echo.Context) error {
	v := dialog.InstallConnectorView{Begin: where(c).base + "/-/new-connector"}
	_, action, manifest, err := h.orch.BeginConnector(
		c.Request().Context(),
		middleware.ScopeOf(c).Org.ID,
		middleware.Principal(c).User.ID,
		c.FormValue("github_org"),
	)
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

// GET /settings/github/callback?code=&state=: GitHub made the app. The
// state's nonce, and the signed-in user matching who began, prove the
// caller began this install. Making the app is not installing it, so the
// browser goes straight on to GitHub's install page; the app's setup_url
// brings it back to githubInstalled. A refused handshake is v0's flash on
// the home canvas; a real failure is the error page.
func (h *handler) githubCallback(c echo.Context) error {
	ctx := c.Request().Context()
	k, err := h.orch.CompleteConnector(ctx, middleware.Principal(c).User.ID, c.QueryParam("state"), c.QueryParam("code"))
	if err != nil {
		var he *echo.HTTPError
		if !errors.As(middleware.HTTPError(err), &he) {
			return err
		}
		hamrmw.SetFlash(c, "GitHub connection failed: "+fmt.Sprint(he.Message), hamrmw.FlashError)
		return c.Redirect(http.StatusSeeOther, "/")
	}
	var install string
	if k.OrgID == nil {
		install, err = h.orch.ServerConnectorInstallURL(ctx, k.ID)
	} else {
		install, err = h.orch.ConnectorInstallURL(ctx, *k.OrgID, k.ID)
	}
	if err != nil {
		return middleware.HTTPError(err)
	}
	return c.Redirect(http.StatusSeeOther, install)
}

// GET /settings/github/installed/:connector?setup_action=: GitHub's setup_url,
// after an install or a change of its repos. Back to where the install shows:
// the server wizard's GitHub step while the server is unfinished, else the
// admin drawer's Connectors tab for a server connector, the setup
// wizard's connector step while the org is unfinished (only the wizard
// reaches it), else the connector drawer's Repos tab. Each of those checks
// access itself; an org the user is not in goes home.
func (h *handler) githubInstalled(c echo.Context) error {
	ctx := c.Request().Context()
	id := c.Param("connector")
	orgID, err := h.orch.ConnectorOrgID(ctx, id)
	if errors.Is(err, errs.ErrNotFound) {
		return c.Redirect(http.StatusSeeOther, "/")
	}
	if err != nil {
		return middleware.HTTPError(err)
	}
	hamrmw.SetFlash(c, "GitHub App installed.", hamrmw.FlashSuccess)
	if orgID == nil {
		s, err := h.orch.ServerSetupState(ctx)
		if err != nil {
			return middleware.HTTPError(err)
		}
		if !s.Done {
			return c.Redirect(http.StatusSeeOther, "/-/setup/server/ui/github")
		}
		return c.Redirect(http.StatusSeeOther, "/?drawer=admin&tab=connectors")
	}
	orgs, err := h.orch.Orgs(ctx, middleware.Principal(c).User.ID)
	if err != nil {
		return middleware.HTTPError(err)
	}
	for _, o := range orgs {
		if o.ID != *orgID {
			continue
		}
		if o.SetupDoneAt == nil {
			return c.Redirect(http.StatusSeeOther, "/"+o.Slug+"/-/setup/connector")
		}
		return c.Redirect(http.StatusSeeOther, "/"+o.Slug+"?drawer=connector:"+id+"&tab=repos")
	}
	return c.Redirect(http.StatusSeeOther, "/")
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
	}{
		{"/:org", "stack", "stack.create"},
		{"/:org/:stack", "env", "env.write"},
	} {
		site.GET(cr.page+"/-/new-"+cr.kind, h.newLevel(cr.kind), a.Require(cr.verb))
		site.POST(cr.page+"/-/new-"+cr.kind, h.createLevel(cr.kind), a.Require(cr.verb))
	}
	site.GET("/:org/-/new-connector", h.newConnector, a.Require("connector.write"))
	site.POST("/:org/-/new-connector", h.beginConnector, a.Require("connector.write"))
	site.GET("/settings/github/callback", h.githubCallback, a.LoginFirst(), a.Authed())
	site.GET("/settings/github/installed/:connector", h.githubInstalled, a.LoginFirst(), a.Authed())
}
