package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	ui "github.com/FyrmForge/stackr/internal/ui/drawer/admin"
	"github.com/FyrmForge/stackr/internal/web/render"
)

var nodeTabs = []string{"overview", "envs", "network", "tags", "security"}

const nodesBase = ui.Base + "/nodes"

// mountNodes is the Nodes tab, the add-node steps and the node drawer. Every
// verb is node.manage, so the route's Require is the whole gate.
func (h *handler) mountNodes(g *echo.Group, a *middleware.Access) {
	m := a.Require("node.manage")
	g.GET(nodesBase+"/add", h.addNode, m)
	g.POST(nodesBase+"/add", h.mintNode, m)
	g.GET(nodesBase+"/add/poll", h.pollNode, m)
	g.POST(nodesBase+"/pending/:node/confirm", h.act("nodes", func(c echo.Context) (string, *service.Job, error) {
		n, err := h.orch.ConfirmJoin(c.Request().Context(), middleware.Principal(c), c.Param("node"))
		return n.Name + " added.", nil, err
	}), m)
	g.POST(nodesBase+"/pending/:node/reject", h.act("nodes", func(c echo.Context) (string, *service.Job, error) {
		return "Rejected.", nil, h.orch.RejectJoin(c.Request().Context(), middleware.Principal(c), c.Param("node"))
	}), m)
	g.POST(nodesBase+"/lost/restore", h.act("nodes", func(c echo.Context) (string, *service.Job, error) {
		_, err := h.orch.RestoreLostEnv(c.Request().Context(), middleware.Principal(c), c.FormValue("env"), c.FormValue("node"))
		return "Redeploying.", nil, err
	}), m)
	g.GET(nodesBase+"/:node", h.nodeDrawer, m)
	g.POST(nodesBase+"/:node/tags", h.nodeAct("tags", "Saved.", func(c echo.Context) error {
		f, err := c.FormParams()
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "bad form")
		}
		return h.orch.SetNodeTags(c.Request().Context(), middleware.Principal(c), c.Param("node"), f["tag"])
	}), m)
	g.POST(nodesBase+"/:node/orgs", h.nodeAct("tags", "Saved.", func(c echo.Context) error {
		f, err := c.FormParams()
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "bad form")
		}
		return h.orch.SetNodeOrgs(c.Request().Context(), middleware.Principal(c), c.Param("node"), f["org"])
	}), m)
	g.POST(nodesBase+"/:node/remove", h.leave("Node removed.", func(c echo.Context) error {
		return h.orch.RemoveNode(c.Request().Context(), middleware.Principal(c), c.Param("node"))
	}), m)
	g.POST(nodesBase+"/:node/forget", h.leave("Node forgotten.", func(c echo.Context) error {
		return h.orch.ForgetNode(c.Request().Context(), middleware.Principal(c), c.Param("node"), c.FormValue("confirm"))
	}), m)
}

// leave runs an action that ends the node, then answers the Nodes tab; a
// refusal answers the node's own drawer.
func (h *handler) leave(note string, do func(echo.Context) error) echo.HandlerFunc {
	return func(c echo.Context) error {
		err := do(c)
		if err != nil {
			return h.nodeAnswer(c, "security", "", err)
		}
		return h.answer(c, "nodes", note, extra{}, nil)
	}
}

// nodeAct runs an action and answers the node drawer on a tab.
func (h *handler) nodeAct(tab, note string, do func(echo.Context) error) echo.HandlerFunc {
	return func(c echo.Context) error {
		err := do(c)
		if err != nil {
			note = ""
		}
		return h.nodeAnswer(c, tab, note, err)
	}
}

// GET /-/admin/nodes/add
func (h *handler) addNode(c echo.Context) error {
	return h.answer(c, "nodes", "", extra{adder: h.step1("", nil, "")}, nil)
}

func (h *handler) step1(name string, picked []string, msg string) *ui.AddNodeView {
	return &ui.AddNodeView{
		Step: 1, Name: name, Picked: picked, Tags: service.NodeTags, Error: msg,
		Create: nodesBase + "/add", Cancel: ui.Base + "?tab=nodes",
	}
}

// POST /-/admin/nodes/add
func (h *handler) mintNode(c echo.Context) error {
	f, err := c.FormParams()
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad form")
	}
	name := strings.TrimSpace(f.Get("name"))
	out, err := h.orch.CreateJoinToken(c.Request().Context(), middleware.Principal(c), service.JoinTokenSpec{
		NodeName: name, Tags: f["tag"],
	})
	if err != nil {
		msg, ok := render.Refused(err)
		if !ok {
			return middleware.HTTPError(err)
		}
		return h.answer(c, "nodes", "", extra{adder: h.step1(name, f["tag"], msg)}, nil)
	}
	return h.answer(c, "nodes", "", extra{adder: &ui.AddNodeView{
		Step: 2, Name: out.NodeName, Command: out.Command,
		Expires: "in " + fmt.Sprint(int(time.Until(out.ExpiresAt).Round(time.Minute).Minutes())) + " minutes",
		Poll:    nodesBase + "/add/poll?name=" + url.QueryEscape(out.NodeName),
		Cancel:  ui.Base + "?tab=nodes",
	}}, nil)
}

// GET /-/admin/nodes/add/poll: 204 until the machine has called in, then
// step three in place of step two.
func (h *handler) pollNode(c echo.Context) error {
	p, err := h.orch.PendingNodeByName(c.Request().Context(), c.QueryParam("name"))
	if errors.Is(err, errs.ErrNotFound) {
		return c.NoContent(http.StatusNoContent)
	}
	if err != nil {
		return middleware.HTTPError(err)
	}
	sys := p.Facts.Kernel
	if p.Facts.CPUs > 0 {
		sys += fmt.Sprintf(" · %d cores · %s", p.Facts.CPUs, gb(p.Facts.MemBytes))
	}
	return respond.HTML(c, http.StatusOK, ui.AddNode(ui.AddNodeView{
		Step: 3, Name: p.Name,
		Pending: &ui.PendingRow{
			Name: p.Name, System: sys, Addresses: p.Addresses,
			Confirm: nodesBase + "/pending/" + p.ID + "/confirm",
			Reject:  nodesBase + "/pending/" + p.ID + "/reject",
		},
	}))
}

func gb(b uint64) string { return fmt.Sprintf("%.0f GB", float64(b)/(1<<30)) }

func (h *handler) nodesTab(c echo.Context, adder *ui.AddNodeView) (templ.Component, error) {
	ctx := c.Request().Context()
	ns, err := h.orch.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	lost, err := h.orch.LostEnvs(ctx)
	if err != nil {
		return nil, err
	}
	v := ui.NodesView{Add: nodesBase + "/add", Adder: adder}
	for _, n := range ns {
		v.Rows = append(v.Rows, ui.NodeRow{
			Name: n.Name, Role: n.Role, Status: n.Status, Tags: n.Tags, Envs: n.Envs,
			Open: nodesBase + "/" + n.ID,
		})
	}
	for _, l := range lost {
		r := ui.LostEnvRow{Path: l.Path, Node: l.Node, Volumes: l.Volumes, EnvID: l.ID, Action: nodesBase + "/lost/restore"}
		for _, n := range ns {
			if n.Status == "online" {
				r.Nodes = append(r.Nodes, ui.Option{Value: n.ID, Label: n.Name})
			}
		}
		v.Lost = append(v.Lost, r)
	}
	return ui.Nodes(v), nil
}

// GET /-/admin/nodes/:node?tab=
func (h *handler) nodeDrawer(c echo.Context) error {
	return h.nodeAnswer(c, c.QueryParam("tab"), "", nil)
}

func (h *handler) nodeAnswer(c echo.Context, tab, note string, actErr error) error {
	ctx := c.Request().Context()
	d, err := h.orch.NodeDetail(ctx, c.Param("node"))
	if err != nil {
		return middleware.HTTPError(err)
	}
	if !contains(nodeTabs, tab) {
		tab = nodeTabs[0]
	}
	base := nodesBase + "/" + d.ID
	f := comp.DrawerView{
		Node: "node:" + d.ID, Title: d.Name, Kind: d.Role, Base: base, Tabs: nodeTabs, Tab: tab,
		Scope: "instance", Note: note,
		Actions: ui.NodeBack(ui.Base + "?tab=nodes"),
	}
	status := http.StatusOK
	if actErr != nil {
		msg, ok := render.Refused(actErr)
		if !ok {
			return middleware.HTTPError(actErr)
		}
		f.Error, status = msg, http.StatusUnprocessableEntity
	}
	v, err := h.nodeView(c, d, base)
	if err != nil {
		return middleware.HTTPError(err)
	}
	return respond.HTML(c, status, comp.Drawer(f, ui.NodeBody(tab, v)))
}

func (h *handler) nodeView(c echo.Context, d service.NodeDetail, base string) (ui.NodeView, error) {
	v := ui.NodeView{
		ID: d.ID, Name: d.Name, Role: d.Role, Status: d.Status, Controller: d.Controller, Tags: d.Tags,
		Range: d.Range, WGAddress: d.WGAddress, AllTags: service.NodeTags, TagsAction: base + "/tags",
	}
	if f := d.Facts; f != nil {
		v.HasFacts = true
		v.Hardware = fmt.Sprintf("%d cores · %s", f.CPUs, gb(f.MemBytes))
		v.Kernel, v.Docker = f.Kernel, f.Docker
		v.Uptime = (time.Duration(f.UptimeS) * time.Second).Round(time.Hour).String()
		v.Disk = render.Size(int64(f.DiskFree)) + " free"
		for _, i := range f.Interfaces {
			kind := "private network"
			switch {
			case i.Public:
				kind = "public"
			case strings.HasPrefix(i.Name, "wg"):
				kind = "cluster"
			}
			for _, p := range i.Prefixes {
				v.Ifaces = append(v.Ifaces, ui.IfaceRow{Name: i.Name, Addr: p, Kind: kind})
			}
		}
	}
	if d.Controller {
		v.RoutesNote = fmt.Sprintf("%s serves the panel and %d routes.", d.Name, d.RoutesServed)
	} else {
		v.OrgsAction = base + "/orgs"
	}
	for _, e := range d.EnvList {
		v.Envs = append(v.Envs, ui.NodeEnvRow{Path: e.Path, Pin: e.Pin, Tiles: e.Tiles})
	}
	for _, o := range d.Orgs {
		v.Orgs = append(v.Orgs, ui.NodeOrgRow{ID: o.ID, Name: o.Name, On: o.On})
	}
	for _, ce := range d.Certs {
		st := "valid"
		if ce.Revoked {
			st = "revoked"
		} else if time.Now().After(ce.NotAfter) {
			st = "expired"
		}
		v.Certs = append(v.Certs, ui.CertRow{
			Serial: ce.Serial, State: st,
			Issued: ce.IssuedAt.Format("Jan 2, 2006"), Expires: ce.NotAfter.Format("Jan 2, 2006"),
			RenewFrom: ce.RenewFrom.Format("Jan 2"),
		})
	}
	if !d.Controller {
		lost, err := h.orch.ForgetPreview(c.Request().Context(), d.ID)
		if err != nil {
			return v, err
		}
		vals, _ := json.Marshal(map[string]string{"confirm": d.Name})
		fb := ui.ForgetBody{Name: d.Name}
		for _, e := range lost {
			fb.Lost = append(fb.Lost, e.Path)
		}
		v.Remove = comp.ConfirmView{
			Button: "Remove node", Title: "Remove " + d.Name + "?",
			Warning: "Its certificate is revoked and it leaves the cluster. It must have no envs.",
			Action:  base + "/remove", Target: "#" + comp.DrawerRoot,
		}
		v.Forget = comp.ConfirmView{
			Button: "Forget node", Title: "Forget " + d.Name + "?",
			Warning: "Use this only if " + d.Name + " is gone for good.",
			Kept:    []string{"its envs' rows and settings, listed as lost until you redeploy them"},
			Word:    d.Name, Action: base + "/forget", Target: "#" + comp.DrawerRoot,
			Vals: string(vals),
			Body: ui.ForgetWarning(fb),
		}
	}
	return v, nil
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
