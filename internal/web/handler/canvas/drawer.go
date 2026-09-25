package canvas

import (
	"errors"
	"fmt"
	"net/http"
	neturl "net/url"
	"slices"
	"strings"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	connui "github.com/FyrmForge/stackr/internal/ui/drawer/connector"
	envui "github.com/FyrmForge/stackr/internal/ui/drawer/env"
	orgui "github.com/FyrmForge/stackr/internal/ui/drawer/org"
	stackui "github.com/FyrmForge/stackr/internal/ui/drawer/stack"
	ui "github.com/FyrmForge/stackr/internal/ui/graph"
)

// A card's drawer lives at the card's own path, so the access middleware
// resolves and checks its scope: /:org/-/drawer (org card), /:org/:stack/-/drawer,
// /:org/:stack/:env/-/drawer, /:org/-/connectors/:connector, and <level>/-/vars
// (the vars card). Tabs are ?tab=; each tab's own verb is checked in body.
type card struct {
	kind string // org | stack | env | connector | vars
	s    service.Scope
	conn service.Connector
}

func urlOf(s service.Scope) string {
	u := "/" + s.Org.Slug
	if s.Stack != nil {
		u += "/" + s.Stack.Slug
	}
	if s.Env != nil {
		u += "/" + s.Env.Slug
	}
	return u
}

func (cd card) frame(tab string) comp.DrawerView {
	s := cd.s
	f := comp.DrawerView{Kind: cd.kind, Base: urlOf(s) + "/-/drawer"}
	switch cd.kind {
	case "org":
		f.Node, f.Title, f.Tabs = "org:"+s.Org.ID, s.Org.Name, orgui.Tabs
	case "stack":
		f.Node, f.Title, f.Tabs = "stack:"+s.Stack.ID, s.Stack.Name, stackui.Tabs
	case "env":
		f.Node, f.Title, f.Tabs = "env:"+s.Env.ID, s.Env.Name, envui.Tabs
	case "connector":
		f.Node, f.Title, f.Tabs = "connector:"+cd.conn.ID, cd.conn.Name, connui.Tabs
		f.Base = urlOf(s) + "/-/connectors/" + cd.conn.ID
	case "vars":
		f.Node = "vars"
		f.Title = "Environment variables"
		f.Icon = "vars"
		f.Tabs = []string{"editor"}
		f.Base = urlOf(s) + "/-/vars"
	}
	f.Tab = tab
	if !slices.Contains(f.Tabs, tab) {
		f.Tab = f.Tabs[0]
	}
	return f
}

// cardOf is the drawer a route names, from what the middleware resolved.
func (h *handler) cardOf(c echo.Context, kind string) (card, error) {
	cd := card{kind: kind, s: middleware.ScopeOf(c)}
	if kind == "connector" {
		return h.connectorByID(c, cd, c.Param("connector"))
	}
	return cd, nil
}

// can asks the one gate about another verb on the card's org.
func can(c echo.Context, s service.Scope, v authz.Verb) bool {
	p := middleware.Principal(c)
	return p != nil && authz.Can(p.Access, v, authz.Resource{OrgID: s.Org.ID}) == nil
}

// refused is the message of an error the viewer can act on (a 4xx other
// than not found), shown over the drawer; ok false = a real failure.
func refused(err error) (string, bool) {
	var he *echo.HTTPError
	if errors.As(middleware.HTTPError(err), &he) && he.Code < 500 && he.Code != http.StatusNotFound {
		return fmt.Sprint(he.Message), true
	}
	return "", false
}

// reload reads the scope's rows again after an action changed them.
func (h *handler) reload(c echo.Context, s service.Scope) (service.Scope, error) {
	var st, en string
	if s.Stack != nil {
		st = s.Stack.Slug
	}
	if s.Env != nil {
		en = s.Env.Slug
	}
	return h.orch.Resolve(c.Request().Context(), s.Org.Slug, st, en, "")
}

// drawerRoute is GET for one kind: the tab ?tab= names.
func (h *handler) drawerRoute(kind string) echo.HandlerFunc {
	return func(c echo.Context) error {
		cd, err := h.cardOf(c, kind)
		if err != nil {
			return middleware.HTTPError(err)
		}
		return h.show(c, http.StatusOK, cd, cd.frame(c.QueryParam("tab")))
	}
}

func (h *handler) show(c echo.Context, status int, cd card, f comp.DrawerView) error {
	d, err := h.render(c, cd, f)
	if err != nil {
		return middleware.HTTPError(err)
	}
	return respond.HTML(c, status, d)
}

// render is the drawer with its tab: the route and a fresh load both come
// here, so a tab's verb is checked in one place.
func (h *handler) render(c echo.Context, cd card, f comp.DrawerView) (templ.Component, error) {
	var body templ.Component
	var err error
	switch cd.kind {
	case "org":
		body, err = h.orgTab(c, cd, &f)
	case "stack":
		body, err = h.stackTab(c, cd, &f)
	case "env":
		body, err = h.envTab(c, cd, &f)
	case "connector":
		body, err = h.connectorTab(c, cd, &f)
	case "vars":
		body, err = h.vars(c, cd.s, "", "")
	}
	if err != nil {
		return nil, err
	}
	return comp.Drawer(f, body), nil
}

// after answers an action with the tab it belongs to: a refusal shows over
// it (422), a done action says what it did.
func (h *handler) after(c echo.Context, cd card, tab, note string, err error) error {
	f := cd.frame(tab)
	if err != nil {
		msg, ok := refused(err)
		if !ok {
			return middleware.HTTPError(err)
		}
		f.Error = msg
		return h.show(c, http.StatusUnprocessableEntity, cd, f)
	}
	// the action may have changed the rows the middleware loaded
	if cd.s, err = h.reload(c, cd.s); err != nil {
		return middleware.HTTPError(err)
	}
	f.Note = note
	return h.show(c, http.StatusOK, cd, f)
}

// drawer is the drawer a fresh load of ?drawer=<node id>&tab= opens: the
// node comes from the canvas the viewer was allowed to draw, its scope is
// resolved from its slug. Nil (the page alone) for a card with no drawer
// here or one the viewer may not open.
func (h *handler) drawer(c echo.Context, v ui.View, id, tab string) (templ.Component, error) {
	var n *ui.Node
	for i := range v.Nodes {
		if v.Nodes[i].ID == id {
			n = &v.Nodes[i]
		}
		for _, s := range v.Nodes[i].Subs {
			if s.ID == id && s.Drawer != "" {
				return envDrawerLoad(s.Drawer, tab), nil
			}
		}
	}
	if n == nil {
		return nil, nil
	}
	cd := card{kind: n.Kind, s: middleware.ScopeOf(c)}
	ctx, s := c.Request().Context(), cd.s
	var err error
	switch n.Kind {
	case "org":
		cd.s, err = h.orch.Resolve(ctx, n.Slug, "", "", "")
	case "stack":
		cd.s, err = h.orch.Resolve(ctx, s.Org.Slug, n.Slug, "", "")
	case "env":
		cd.s, err = h.orch.Resolve(ctx, s.Org.Slug, s.Stack.Slug, n.Slug, "")
	case "connector":
		cd, err = h.connectorByID(c, cd, n.Slug)
	case "vars":
		if s.Org == nil {
			return nil, nil
		}
	default: // the env canvas's kinds: their own route loads into the open drawer
		if n.Drawer == "" {
			return nil, nil
		}
		return envDrawerLoad(n.Drawer, tab), nil
	}
	if err != nil || !can(c, cd.s, "org.read") {
		return nil, ignoreRefusal(err)
	}
	return h.render(c, cd, cd.frame(tab))
}

func (h *handler) connectorByID(c echo.Context, cd card, id string) (card, error) {
	cs, err := h.orch.Connectors(c.Request().Context(), cd.s.Org.ID)
	for _, k := range cs {
		if k.ID == id {
			cd.conn = k
		}
	}
	if err == nil && cd.conn.ID == "" {
		err = errs.ErrNotFound
	}
	return cd, err
}

func ignoreRefusal(err error) error {
	if _, ok := refused(err); ok || errors.Is(err, errs.ErrNotFound) {
		return nil
	}
	return err
}

// envDrawerLoad opens the drawer with its route fetched on load (url
// carries the card's default ?tab=; tab, when set, wins), so the route's
// own middleware and render answer it.
// ponytail: one extra request; render server-side if the flash shows.
func envDrawerLoad(url, tab string) templ.Component {
	if tab != "" {
		url, _, _ = strings.Cut(url, "?")
		url += "?tab=" + neturl.QueryEscape(tab)
	}
	return comp.DrawerLoad(url)
}
