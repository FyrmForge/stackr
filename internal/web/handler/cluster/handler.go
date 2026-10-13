// Package cluster serves the Cluster map (lane U2): /-/cluster, the admin's
// canvas of nodes, networks, tunnels, public arrows and backups. It is the
// same canvas as the org, stack and env graphs (pan, zoom, notes); networks
// and nodes are placed from the service's facts and stay where an admin
// drops them.
// Under "/-/cluster" it keeps the canvas's own helper routes ("/-/notes",
// "/-/positions", "/-/reset") and "/map", the timer's refresh.
package cluster

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/ui/cluster"
	"github.com/FyrmForge/stackr/internal/ui/components"
	ui "github.com/FyrmForge/stackr/internal/ui/graph"
	"github.com/FyrmForge/stackr/internal/web/render"
)

type handler struct{ orch *service.Orchestrator }

func NewHandler(orch *service.Orchestrator) *handler { return &handler{orch: orch} }

// Mount registers the page and its helper routes. Only an admin may manage
// nodes, so only an admin sees the map.
func (h *handler) Mount(g *echo.Group, a *middleware.Access) {
	admin := a.Require("node.manage")
	g.GET(cluster.Base, h.Page, a.LoginFirst(), admin)
	g.GET(cluster.Base+"/map", h.Map, admin)
	g.POST(cluster.Base+"/-/positions", h.Positions, admin)
	g.POST(cluster.Base+"/-/reset", h.Reset, admin)
	g.POST(cluster.Base+"/-/notes", h.Notes, admin)
	g.POST(cluster.Base+"/-/notes/delete", h.DeleteNote, admin)
}

func (h *handler) view(c echo.Context) (ui.View, cluster.Map, error) {
	ctx := c.Request().Context()
	cm, err := h.orch.ClusterMap(ctx)
	if err != nil {
		return ui.View{}, cluster.Map{}, err
	}
	as, err := h.orch.Annotations(ctx, service.ClusterScope)
	if err != nil {
		return ui.View{}, cluster.Map{}, err
	}
	m := mapMap(cm)
	placed, err := h.orch.ClusterPositions(ctx)
	if err != nil {
		return ui.View{}, cluster.Map{}, err
	}
	m.Placed = map[string][2]int{}
	for id, p := range placed {
		m.Placed[id] = [2]int{p.X, p.Y}
	}
	var notes []ui.Note
	for _, a := range as {
		notes = append(notes, ui.Note{ID: a.ID, Kind: a.Kind, Text: a.Text, X: a.X, Y: a.Y, W: a.W, H: a.H})
	}
	create := []ui.Create{{Label: "+ Add node", URL: cluster.AddNodeURL}}
	return cluster.Build(m, cluster.Base, notes, create), m, nil
}

// GET /-/cluster. A fresh load of ?drawer=node:<id>&tab= comes with that
// node's drawer open (lane U1 serves it at cluster.NodeDrawer).
func (h *handler) Page(c echo.Context) error {
	v, m, err := h.view(c)
	if err != nil {
		return middleware.HTTPError(err)
	}
	var drawer templ.Component
	if id, ok := strings.CutPrefix(c.QueryParam("drawer"), "node:"); ok && id != "" {
		drawer = components.DrawerLoad(cluster.NodeDrawer(id, c.QueryParam("tab")))
	}
	return render.PageWith(c, http.StatusOK, "Cluster", cluster.Page(v, m), nil, drawer)
}

// GET /-/cluster/map: the canvas alone, for the timer's swap into #graph.
func (h *handler) Map(c echo.Context) error {
	v, _, err := h.view(c)
	if err != nil {
		return middleware.HTTPError(err)
	}
	return respond.HTML(c, http.StatusOK, ui.Canvas(v))
}

// POST …/-/positions (node_id, x, y): a note, node, NAT, internet or backup
// card saves where it was dropped. A network box is where its cards are
// (each of them posts its own drop), so its drop is not kept.
func (h *handler) Positions(c echo.Context) error {
	id := c.FormValue("node_id")
	x, errX := strconv.Atoi(c.FormValue("x"))
	y, errY := strconv.Atoi(c.FormValue("y"))
	if errX != nil || errY != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "x and y are whole numbers")
	}
	if strings.HasPrefix(id, "net:") {
		// Its place follows its cards; a hand-set size (w, h) is kept.
		w, errW := strconv.Atoi(c.FormValue("w"))
		hh, errH := strconv.Atoi(c.FormValue("h"))
		if errW != nil || errH != nil || w <= 0 || hh <= 0 {
			return c.NoContent(http.StatusNoContent)
		}
		if err := h.orch.PlaceCluster(c.Request().Context(), "size:"+id, service.Point{X: w, Y: hh}); err != nil {
			return middleware.HTTPError(err)
		}
		return c.NoContent(http.StatusNoContent)
	}
	if !strings.HasPrefix(id, "note:") && !cluster.Movable(id) {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "this card does not move")
	}
	if err := h.orch.PlaceCluster(c.Request().Context(), id, service.Point{X: x, Y: y}); err != nil {
		return middleware.HTTPError(err)
	}
	return c.NoContent(http.StatusNoContent)
}

// POST …/-/reset: every network and node back to its computed place.
func (h *handler) Reset(c echo.Context) error {
	if err := h.orch.ResetPositions(c.Request().Context(), service.ClusterScope); err != nil {
		return middleware.HTTPError(err)
	}
	return h.Map(c)
}

// POST …/-/notes (id?, kind, text, x, y, w, h): create or save a note or
// box; an emptied note is deleted. Answers the canvas.
func (h *handler) Notes(c echo.Context) error {
	var n [4]int
	for i, k := range []string{"x", "y", "w", "h"} {
		if v := c.FormValue(k); v != "" {
			var err error
			if n[i], err = strconv.Atoi(v); err != nil {
				return echo.NewHTTPError(http.StatusBadRequest, k+" is a whole number")
			}
		}
	}
	a := service.Annotation{
		ID: c.FormValue("id"), Kind: c.FormValue("kind"), Text: c.FormValue("text"),
		X: n[0], Y: n[1], W: n[2], H: n[3],
	}
	if _, _, err := h.orch.SetAnnotation(c.Request().Context(), service.ClusterScope, a); err != nil {
		return middleware.HTTPError(err)
	}
	return h.Map(c)
}

// POST …/-/notes/delete (id).
func (h *handler) DeleteNote(c echo.Context) error {
	if err := h.orch.DeleteAnnotation(c.Request().Context(), service.ClusterScope, c.FormValue("id")); err != nil {
		return middleware.HTTPError(err)
	}
	return h.Map(c)
}

// mapMap is the service's facts as the page draws them.
func mapMap(cm service.ClusterMap) cluster.Map {
	var m cluster.Map
	for _, n := range cm.Nodes {
		u := cluster.Node{
			ID: n.ID, Name: n.Name, Role: n.Role, Status: n.Status, Docker: n.Docker, Net: n.Net,
			Tags: n.Tags, Public: n.Public, Domains: n.Domains, DomainEnvs: n.DomainEnvs, ExitEnvs: n.ExitEnvs, NoForward: n.NoForward, ReachedVia: n.ReachedVia, ViaDown: n.ViaDown,
		}
		for _, e := range n.Envs {
			u.Envs = append(u.Envs, cluster.Env{Name: e.Path, Href: e.Href, Health: e.Health})
		}
		m.Nodes = append(m.Nodes, u)
	}
	for _, n := range cm.Networks {
		m.Nets = append(m.Nets, cluster.Net{ID: n.ID, CIDR: n.CIDR, Nodes: n.Nodes, Router: n.Router, Forward: n.Forward, Unsure: n.Unsure})
	}
	for _, l := range cm.Links {
		m.Links = append(m.Links, cluster.Link{A: l.A, B: l.B, Kind: l.Kind, MS: l.MS, Via: l.Via, Down: l.Down})
	}
	for _, b := range cm.Backups {
		m.Backups = append(m.Backups, cluster.Backup{ID: b.ID, Name: b.Name, Nodes: b.Nodes})
	}
	return m
}
