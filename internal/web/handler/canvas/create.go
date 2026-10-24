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
	"github.com/FyrmForge/stackr/internal/ui/connectors"
	"github.com/FyrmForge/stackr/internal/ui/dialog"
	ui "github.com/FyrmForge/stackr/internal/ui/graph"
	"github.com/FyrmForge/stackr/internal/web/connpanel"
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
		// an owner gets the Connectors tab's add dialog (every kind); a
		// writer, who has no such tab, the git kinds' add dialog
		if cr.kind == "connector" && authz.Can(p.Access, "dns.manage", r) == nil {
			out = append(out, ui.Create{Label: cr.label, URL: l.base + "/-/drawer?tab=connectors&add=1"})
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

// GET /:org/-/new-connector: a writer's add dialog, on the git kinds (an
// owner's opens the Connectors tab's, every kind): straight on the one
// there is, else its picker. A redirect setup comes back to the org page.
func (h *handler) newConnector(c echo.Context) error {
	p := h.panel()
	p.Back = nil
	p.Return = orgSlug
	ks := p.KindsWith(service.CapGit)
	if len(ks) == 1 {
		return respond.HTML(c, http.StatusOK, p.Open(c, ks[0]))
	}
	return respond.HTML(c, http.StatusOK, connectors.Modal("Add a connector", "", connectors.Picker(p.Kinds(c, service.CapGit))))
}

// installedBack is where a provider's return with no state lands (a change
// made at the provider): the server wizard's git step while the server is
// unfinished, else Admin's Connectors tab, for a server connection; the
// setup wizard's connector step while the org is unfinished, else the
// connector drawer's Repos tab. An org the user is not in goes home.
func (h *handler) installedBack(c echo.Context, orgID *string, id string) (string, error) {
	ctx := c.Request().Context()
	if orgID == nil {
		s, err := h.orch.ServerSetupState(ctx)
		if err != nil {
			return "", err
		}
		if !s.Done {
			return "/-/setup/server/ui/" + service.ServerSetupGit, nil
		}
		return "/?drawer=admin&tab=connectors", nil
	}
	orgs, err := h.orch.Orgs(ctx, middleware.Principal(c).User.ID)
	if err != nil {
		return "", err
	}
	for _, o := range orgs {
		if o.ID != *orgID {
			continue
		}
		if o.SetupDoneAt == nil {
			return "/" + o.Slug + "/-/setup/connector", nil
		}
		return "/" + o.Slug + "?drawer=connector:" + id + "&tab=repos", nil
	}
	return "/", nil
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
	// a provider's returns: the callback paths its apps were registered
	// with before the core had one, and its later visits (an install)
	site.GET(service.ConnectorLegacyBack, connpanel.Callback(h.orch), a.LoginFirst(), a.Authed())
	site.GET(service.ConnectorReturnPath, connpanel.Installed(h.orch, h.installedBack), a.LoginFirst(), a.Authed())
}
