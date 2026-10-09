package canvas

import (
	"context"
	"net/http"

	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/ui/access"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	"github.com/FyrmForge/stackr/internal/ui/dialog"
	stackui "github.com/FyrmForge/stackr/internal/ui/drawer/stack"
)

func (h *handler) stackTab(c echo.Context, cd card, f *comp.DrawerView) (templ.Component, error) {
	ctx, st := c.Request().Context(), cd.s.Stack
	g, err := h.orch.HostGrant(ctx, st.ID)
	if err != nil {
		return nil, err
	}
	if len(g.Lines)+len(g.Pending) > 0 {
		f.Scope = "elevated access"
	}
	switch f.Tab {
	case "params":
		return h.vars(c, cd.s, "", "")
	case "releases":
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
	v := stackui.SettingsView{
		Name:      st.Name,
		Slug:      st.Slug,
		Where:     cd.s.Org.Slug + " / " + st.Slug,
		Cascade:   cascade,
		Connector: st.ConfigConnectorID,
		Repo:      st.ConfigRepo,
		Branch:    st.ConfigBranch,
		Path:      st.ConfigPath,
	}
	v.Host = stackui.HostView{Tiles: access.Group(g.Lines, g.Pending)}
	if len(g.Pending) > 0 && can(c, cd.s, "hostgrant.approve") {
		// The set shown rides along; the server refuses when the ask moved.
		addrs, panel := h.orch.HostAddrs()
		v.Host.Approve = &access.ApproveView{
			Addrs:   addrs,
			Panel:   panel,
			ID:      "host-approve",
			Action:  f.Base + "/host-grant/approve",
			Name:    st.Name,
			Pending: g.Pending,
			Tiles:   access.Group(nil, g.Pending),
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
		v.Cascade.ReadOnly, v.Cascade.Why = true, "Changing defaults needs write access to this stack."
		return stackui.Settings(v), nil
	}
	v.Base = f.Base
	v.Delete = dialog.DeleteStack(st.Name, f.Base+"/delete", "#"+comp.DrawerRoot)
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

func (h *handler) mountStack(site *echo.Group, a *middleware.Access) {
	s := "/:org/:stack/-/drawer"
	site.GET(s, h.drawerRoute("stack"), a.Require("org.read"))
	write := a.Require("stack.write")
	site.POST(s+"/rename", h.stackAction("settings", func(c echo.Context, cd card) (string, error) {
		st, err := h.orch.RenameStack(c.Request().Context(), cd.s.Stack.ID, c.FormValue("name"))
		if err != nil {
			return "", err
		}
		return redirect(c, "/"+cd.s.Org.Slug+"?drawer=stack:"+st.ID+"&tab=settings")
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
		return "Config repo saved.", err
	}), write)
	site.POST(s+"/host-grant/approve", h.stackAction("settings", func(c echo.Context, cd card) (string, error) {
		f, _ := c.FormParams()
		pending, grant, msg := access.Read(f, cd.s.Stack.Name)
		if msg != "" {
			return "", errs.Invalidf("grant", "%s", msg)
		}
		_, err := h.orch.ApproveHostGrant(c.Request().Context(), cd.s.Stack.ID, middleware.Principal(c).User.ID, pending, grant)
		return "Elevated access approved.", err
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
		j, err := h.orch.Promote(c.Request().Context(), e.ID, c.Param("release"))
		if err != nil {
			return "", err
		}
		c.Set(jobKey, queued{job: j, page: urlOf(cd.s) + "/" + e.Slug})
		return "Queued: the job below follows it.", nil
	}), a.Require("env.write"))
	site.POST(s+"/delete", h.stackAction("settings", func(c echo.Context, cd card) (string, error) {
		if err := h.orch.DeleteStack(c.Request().Context(), cd.s.Stack.ID); err != nil {
			return "", err
		}
		return redirect(c, "/"+cd.s.Org.Slug)
	}), write)
}
