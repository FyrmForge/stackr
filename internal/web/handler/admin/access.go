package admin

import (
	"cmp"
	"context"
	"net/url"
	"slices"
	"strings"

	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/ui/access"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	ui "github.com/FyrmForge/stackr/internal/ui/drawer/admin"
)

func (h *handler) mountAccess(g *echo.Group, a *middleware.Access) {
	approve := a.Require("hostgrant.approve")
	g.POST(ui.Base+"/access/approve", h.act("access", func(c echo.Context) (string, *service.Job, error) {
		ctx := c.Request().Context()
		stacks, err := h.stackNames(ctx)
		if err != nil {
			return "", nil, err
		}
		st, ok := stacks[c.FormValue("stack")]
		if !ok {
			return "", nil, errs.ErrNotFound
		}
		f, _ := c.FormParams()
		pending, grant, msg := access.Read(f, st.name)
		if msg != "" {
			return "", nil, errs.Invalidf("grant", "%s", msg)
		}
		_, err = h.orch.ApproveHostGrant(ctx, st.id, middleware.Principal(c).User.ID, pending, grant)
		return "Elevated access approved.", nil, err
	}), approve)
	g.POST(ui.Base+"/access/revoke", h.act("access", func(c echo.Context) (string, *service.Job, error) {
		if c.QueryParam("tile") == "" { // "" would revoke the whole stack
			return "", nil, errs.Invalidf("tile", "name a tile")
		}
		err := h.orch.RevokeHostGrant(c.Request().Context(), c.QueryParam("stack"), c.QueryParam("tile"))
		return "Access revoked.", nil, err
	}), approve)
}

type stackName struct{ id, name, slug, org, orgID string }

// stackNames is every stack by id, with its org: a grant names only the
// stack's id.
// ponytail: one pass over all orgs and stacks per tab view; fine for a
// self-hosted server, index by id in the store if it ever is not.
func (h *handler) stackNames(ctx context.Context) (map[string]stackName, error) {
	orgs, err := h.orch.AllOrgs(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]stackName{}
	for _, o := range orgs {
		ss, err := h.orch.Stacks(ctx, o.ID)
		if err != nil {
			return nil, err
		}
		for _, s := range ss {
			out[s.ID] = stackName{id: s.ID, name: s.Name, slug: s.Slug, org: o.Name, orgID: o.ID}
		}
	}
	return out, nil
}

var permFilters = []ui.AccessOption{
	{Value: access.KindNet, Label: "Host networking"},
	{Value: access.KindDocker, Label: "Docker socket"},
	{Value: access.KindPrivileged, Label: "Privileged"},
	{Value: access.KindDevice, Label: "Device"},
	{Value: access.KindLAN, Label: "LAN access"},
	{Value: access.KindPort, Label: "Server port"},
	{Value: access.KindFolder, Label: "Host folder"},
}

// accessTab lists the stacks with a request open and every grant per tile,
// filtered by ?org= (id), ?perm= (kind) and ?q= (any word of the row).
func (h *handler) accessTab(c echo.Context) (templ.Component, error) {
	ctx := c.Request().Context()
	gs, err := h.orch.ListHostGrants(ctx)
	if err != nil {
		return nil, err
	}
	stacks, err := h.stackNames(ctx)
	if err != nil {
		return nil, err
	}
	us, err := h.orch.Users(ctx)
	if err != nil {
		return nil, err
	}
	who := map[string]string{}
	for _, u := range us {
		who[u.ID] = u.Email
	}
	v := ui.AccessView{Filter: ui.AccessFilter{
		Org:   c.QueryParam("org"),
		Perm:  c.QueryParam("perm"),
		Q:     strings.TrimSpace(c.QueryParam("q")),
		Perms: permFilters,
	}}
	seen := map[string]bool{}
	for _, st := range stacks {
		if !seen[st.orgID] {
			seen[st.orgID] = true
			v.Filter.Orgs = append(v.Filter.Orgs, ui.AccessOption{Value: st.orgID, Label: st.org})
		}
	}
	slices.SortFunc(v.Filter.Orgs, func(a, b ui.AccessOption) int { return cmp.Compare(a.Label, b.Label) })
	slices.SortFunc(gs, func(a, b service.HostGrant) int {
		return cmp.Or(cmp.Compare(stacks[a.StackID].org, stacks[b.StackID].org), cmp.Compare(stacks[a.StackID].name, stacks[b.StackID].name))
	})
	for _, g := range gs {
		st, ok := stacks[g.StackID]
		if !ok {
			continue
		}
		where := st.org + " / " + st.name
		if len(g.Pending) > 0 {
			v.Waiting = append(v.Waiting, ui.WaitingStack{Where: where, Form: access.ApproveView{
				ID:      "access-" + g.StackID,
				Action:  ui.Base + "/access/approve",
				StackID: g.StackID,
				Name:    st.name,
				Pending: g.Pending,
				Tiles:   access.Group(nil, g.Pending),
			}})
		}
		if v.Filter.Org != "" && v.Filter.Org != st.orgID {
			continue
		}
		by := cmp.Or(who[g.ApprovedBy], "a removed user")
		for _, t := range access.Group(g.Lines, nil) {
			if !matches(t, where, v.Filter.Perm, v.Filter.Q) {
				continue
			}
			v.Rows = append(v.Rows, ui.AccessRow{
				Org: st.org, Stack: st.name, Tile: t.Tile,
				Perms: t.Perms,
				By:    by,
				When:  g.CreatedAt.Local().Format("Jan 2"),
				Revoke: comp.ConfirmView{
					Button:  "Revoke",
					Title:   "Revoke " + t.Tile + "'s access?",
					Warning: "The running container keeps going; the next deploy waits again.",
					Action:  ui.Base + "/access/revoke?stack=" + url.QueryEscape(g.StackID) + "&tile=" + url.QueryEscape(t.Tile),
					Target:  "#" + comp.DrawerRoot,
				},
			})
		}
	}
	return ui.Access(v), nil
}

// matches keeps a tile whose perms include the kind (if one is picked) and
// whose row text holds the search.
func matches(t access.TilePerms, where, kind, q string) bool {
	if kind != "" && !slices.ContainsFunc(t.Perms, func(p access.Perm) bool { return p.Kind == kind }) {
		return false
	}
	hay := where + " " + t.Tile
	for _, p := range t.Perms {
		hay += " " + p.Label + " " + p.Detail
	}
	return strings.Contains(strings.ToLower(hay), strings.ToLower(q))
}
