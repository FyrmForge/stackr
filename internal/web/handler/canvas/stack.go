package canvas

import (
	"context"
	"net/http"
	neturl "net/url"
	"slices"
	"strings"

	hamrmw "github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/ui/access"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	"github.com/FyrmForge/stackr/internal/ui/dialog"
	stackui "github.com/FyrmForge/stackr/internal/ui/drawer/stack"
	placementui "github.com/FyrmForge/stackr/internal/ui/placement"
	"github.com/FyrmForge/stackr/internal/web/render"
)

func (h *handler) stackTab(c echo.Context, cd card, f *comp.DrawerView) (templ.Component, error) {
	ctx, st := c.Request().Context(), cd.s.Stack
	g, err := h.orch.HostGrant(ctx, st.ID)
	if err != nil {
		return nil, err
	}
	if len(g.Lines)+len(g.Pending) > 0 {
		f.Scope = "host access"
	}
	if f.Tab == "releases" {
		return h.releasesTab(c, cd, f)
	}
	// the org's own connectors and the server ones shared with it
	cs, err := h.orch.ConnectedConnectors(ctx, cd.s.Org.ID)
	if err != nil {
		return nil, err
	}
	cascade, err := h.cascadeForm(c, cd, st.Settings)
	if err != nil {
		return nil, err
	}
	es, err := h.orch.Ladder(ctx, st.ID)
	if err != nil {
		return nil, err
	}
	pv, err := h.orch.Placement(ctx, service.ScopeStack, st.ID)
	if err != nil {
		return nil, err
	}
	vars, err := h.vars(c, cd.s, "", "")
	if err != nil {
		return nil, err
	}
	set, secret, err := h.varCounts(c, cd.s)
	if err != nil {
		return nil, err
	}
	v := stackui.SettingsView{
		Open:          c.FormValue("open"),
		Refresh:       f.Base + "?tab=settings",
		Name:          st.Name,
		Slug:          st.Slug,
		Where:         cd.s.Org.Slug + " / " + st.Slug,
		Order:         ladderView(es, c.FormValue("moved")),
		Cascade:       cascade,
		Connector:     st.ConfigConnectorID,
		Repo:          st.ConfigRepo,
		Branch:        st.ConfigBranch,
		Path:          st.ConfigPath,
		Vars:          comp.VarsSummary(set, secret),
		VarsSet:       set > 0,
		VarsBody:      vars,
		Placement:     stackui.PlacementSummary(pv.Pin, pv.PinName, pv.Ingress),
		PlacementSet:  placementui.Changed(service.ScopeStack, pv.Pin, pv.Ingress, pv.Inherited, len(pv.Rungs)),
		PlacementBody: placementui.Load(urlOf(cd.s) + "/-/placement"),
	}
	v.Host = stackui.HostView{Tiles: access.Group(g.Lines, g.Pending)}
	if len(g.Pending) > 0 && can(c, cd.s, "hostgrant.approve") {
		// The set shown rides along; the server refuses when the ask moved.
		addrs, panel := h.orch.HostAddrs()
		warn, werr := render.ControllerWarn(c.Request().Context(), h.orch, cd.s.Org.Slug, st.ID, st.Slug)
		if werr != nil {
			return nil, werr
		}
		v.Host.Approve = &access.ApproveView{
			Addrs:      addrs,
			Panel:      panel,
			ID:         "host-approve",
			Action:     f.Base + "/host-grant/approve",
			Name:       st.Name,
			Pending:    g.Pending,
			Tiles:      access.Group(nil, g.Pending),
			Controller: warn,
		}
	}
	for _, k := range cs {
		label := k.Name + " (" + k.Host + ")"
		if k.OrgID == nil {
			label += ", server"
		}
		v.Connectors = append(v.Connectors, stackui.Option{Value: k.ID, Label: label})
	}
	if !can(c, cd.s, "stack.write") {
		v.Cascade.ReadOnly, v.Cascade.Why = true, "Only members and owners can change resources."
		return stackui.Settings(v), nil
	}
	v.Base = f.Base
	v.Order.Action = f.Base + "/order"
	v.Delete = dialog.DeleteStack(st.Name, st.Slug, f.Base+"/delete", "#"+comp.DrawerRoot)
	if st.ConfigRepo != "" {
		v.Unbind = comp.ConfirmView{
			Button:  "Unbind",
			Title:   "Unbind the config repository?",
			Warning: "The stack goes back to being managed from the UI. Nothing deployed changes.",
			Action:  f.Base + "/unbind",
			Target:  "#" + comp.DrawerRoot,
		}
	}
	return stackui.Settings(v), nil
}

// ladderView is the stack's envs, bottom rung first, each with where its
// releases come from and the whole order after one move up or down the list.
func ladderView(es []service.Environment, moved string) stackui.OrderView {
	ids := make([]string, len(es))
	for i, e := range es {
		ids[i] = e.ID
	}
	hues := service.EnvHues(es)
	// a move is offered only when the env it puts first may be first
	swap := func(i, j int) []string {
		if j < 0 || j >= len(ids) || (min(i, j) == 0 && !service.CanBeBottom(es[max(i, j)])) {
			return nil
		}
		o := slices.Clone(ids)
		o[i], o[j] = o[j], o[i]
		return o
	}
	v := stackui.OrderView{Moved: moved}
	for i, e := range es {
		d := strings.TrimSpace("branch " + e.FromBranch)
		if e.FromKind != "branch" {
			d = "promoted"
			if i > 0 {
				d = "from " + es[i-1].Name
			}
		}
		if e.Locked {
			d += ", locked"
		}
		v.Rungs = append(v.Rungs, stackui.Rung{
			ID:     e.ID,
			Name:   e.Name,
			Color:  hues[e.ID],
			Detail: d,
			Up:     swap(i, i-1),
			Down:   swap(i, i+1),
		})
	}
	return v
}

func (h *handler) stackAction(tab string, do func(echo.Context, card) (string, error)) echo.HandlerFunc {
	return func(c echo.Context) error {
		cd, _ := h.cardOf(c, "stack")
		note, err := do(c, cd)
		if err == nil && c.Response().Committed {
			return nil
		}
		return h.after(c, cd, tab, note, err)
	}
}

// redirect sends the page to url (a moved slug, a deleted card).
func redirect(c echo.Context, url string) (string, error) {
	c.Response().Header().Set("HX-Redirect", url)
	return "", c.NoContent(http.StatusOK)
}

// renamed answers a rename: a flash, then the page the viewer was on with
// the renamed card's drawer open again. On the card's own page (its old
// URL, own) that is the page at its new URL (moved); anywhere else, its
// card's canvas (canvas). query is the drawer's ?drawer=&tab=&open=. warn
// is what the rename could not carry, read with the flash.
func renamed(c echo.Context, name string, warn []string, own, moved, canvas, query string) (string, error) {
	if len(warn) > 0 {
		hamrmw.SetFlash(c, "Renamed to "+name+". "+strings.Join(warn, " "), hamrmw.FlashInfo)
	} else {
		hamrmw.SetFlash(c, "Renamed to "+name+".", hamrmw.FlashSuccess)
	}
	if cur, err := neturl.Parse(c.Request().Header.Get("HX-Current-URL")); err == nil && cur.Path == own {
		return redirect(c, moved+"?"+query)
	}
	return redirect(c, canvas+"?"+query)
}

func (h *handler) mountStack(site *echo.Group, a *middleware.Access) {
	s := "/:org/:stack/-/drawer"
	site.GET(s, h.drawerRoute("stack"), a.Require("org.read"))
	write := a.Require("stack.write")
	site.POST(s+"/rename", h.stackAction("settings", func(c echo.Context, cd card) (string, error) {
		st, warn, err := h.orch.RenameStack(c.Request().Context(), cd.s.Stack.ID, c.FormValue("name"))
		if err != nil {
			return "", err
		}
		org := "/" + cd.s.Org.Slug
		return renamed(c, st.Name, warn, urlOf(cd.s), org+"/"+st.Slug, org, "drawer=stack:"+st.ID+"&tab=settings&open=general")
	}), write)
	site.POST(s+"/config", h.stackAction("settings", func(c echo.Context, cd card) (string, error) {
		f := c.FormValue
		_, err := h.orch.SetConfigRepo(
			c.Request().Context(),
			cd.s.Stack.ID,
			f("connector"),
			f("repo"),
			f("branch"),
			f("path"),
		)
		return "Saved.", err
	}), write)
	site.POST(s+"/order", h.stackAction("settings", func(c echo.Context, cd card) (string, error) {
		form, _ := c.FormParams()
		return "Saved.", h.orch.ReorderEnvs(c.Request().Context(), cd.s.Stack.ID, form["ids"])
	}), write)
	site.POST(s+"/host-grant/approve", h.stackAction("settings", func(c echo.Context, cd card) (string, error) {
		f, _ := c.FormParams()
		pending, grant, msg := access.Read(f, cd.s.Stack.Name)
		if msg != "" {
			return "", errs.Invalidf("grant", "%s", msg)
		}
		_, err := h.orch.ApproveHostGrant(c.Request().Context(), cd.s.Stack.ID, middleware.Principal(c).User.ID, pending, grant)
		return "Host access approved.", err
	}), a.Require("hostgrant.approve"))
	site.POST(s+"/unbind", h.stackAction("settings", func(c echo.Context, cd card) (string, error) {
		_, err := h.orch.SetConfigRepo(c.Request().Context(), cd.s.Stack.ID, "", "", "", "")
		return "Unbound: the stack is managed from the UI.", err
	}), write)
	site.POST(s+"/settings", h.saveRung(
		"stack",
		func(s service.Scope) string { return s.Stack.Settings },
		func(ctx context.Context, s service.Scope, blob string) error {
			_, err := h.orch.SetStackSettings(ctx, s.Stack.ID, blob)
			return err
		},
	), write)
	// a promote from the stack's releases: :env is a slug, resolved inside this stack
	site.POST(s+"/promote/:env/:release", h.stackAction("releases", func(c echo.Context, cd card) (string, error) {
		e := cd.s.Env
		if e == nil {
			return "", errs.ErrNotFound
		}
		j, err := h.orch.PromoteTicked(c.Request().Context(), middleware.Principal(c), e.ID, c.Param("release"), render.ApproveOpts(c).Ticked)
		if err != nil {
			return "", err
		}
		c.Set(jobKey, queued{job: j, page: urlOf(cd.s)})
		return "Queued: the job below follows it.", nil
	}), a.Require("env.write"))
	site.POST(s+"/delete", h.stackAction("settings", func(c echo.Context, cd card) (string, error) {
		j, err := h.orch.TeardownStack(c.Request().Context(), cd.s.Stack.ID, c.FormValue("confirm"))
		if err != nil {
			return "", err
		}
		if j.ID != "" {
			hamrmw.SetFlash(c, "Removing "+cd.s.Stack.Name+": its tiles and envs go first, then the stack.", hamrmw.FlashInfo)
		}
		return redirect(c, "/"+cd.s.Org.Slug)
	}), write)
}
