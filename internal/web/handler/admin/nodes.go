package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	clusterui "github.com/FyrmForge/stackr/internal/ui/cluster"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	ui "github.com/FyrmForge/stackr/internal/ui/drawer/admin"
	"github.com/FyrmForge/stackr/internal/web/render"
)

var nodeTabs = []string{"overview", "metrics", "envs", "network", "tags", "security"}

const nodesBase = ui.Base + "/nodes"

// breakGlassWord is what the admin types to confirm a break glass.
const breakGlassWord = "break glass"

// mountNodes is the Nodes tab, the add-node steps and the node drawer. Every
// verb is node.manage, so the route's Require is the whole gate.
func (h *handler) mountNodes(g *echo.Group, a *middleware.Access) {
	m := a.Require("node.manage")
	h.mountPlacements(g, a)
	g.GET(nodesBase+"/add", h.addNode, m)
	g.POST(nodesBase+"/add", h.mintNode, m)
	g.GET(nodesBase+"/add/poll", h.pollNode, m)
	g.POST(nodesBase+"/pending/:node/confirm", h.act("nodes", func(c echo.Context) (string, *service.Job, error) {
		ctx := c.Request().Context()
		n, err := h.orch.ConfirmJoin(ctx, middleware.Principal(c), c.Param("node"))
		if err != nil {
			return "", nil, err
		}
		// a node the bound server file names queues a re-plan, so a
		// placement waiting on it gets its node without a push
		if b, berr := h.orch.ServerConfigBinding(ctx); berr == nil && b.Repo != "" {
			return n.Name + " added. The server file is re-planning.", nil, nil
		}
		return n.Name + " added.", nil, nil
	}), m)
	g.POST(nodesBase+"/pending/:node/reject", h.act("nodes", func(c echo.Context) (string, *service.Job, error) {
		return "Rejected.", nil, h.orch.RejectJoin(c.Request().Context(), middleware.Principal(c), c.Param("node"))
	}), m)
	g.GET(nodesBase+"/:node", h.nodeDrawer, m)
	g.GET(nodesBase+"/:node/metrics/events", h.nodeMetricsEvents, m)
	g.POST(nodesBase+"/:node/tags", h.nodeAct("tags", "Saved.", func(c echo.Context) error {
		f, err := c.FormParams()
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "bad form")
		}
		var exits []string
		for _, x := range f["exit"] {
			if x != "" {
				exits = append(exits, x)
			}
		}
		return h.orch.SaveNodeTags(c.Request().Context(), middleware.Principal(c), c.Param("node"), f["tag"], exits)
	}), m)
	g.POST(nodesBase+"/default-ingress/preview", h.previewIngress, m)
	g.POST(nodesBase+"/default-ingress", h.act("nodes", func(c echo.Context) (string, *service.Job, error) {
		_, err := h.orch.SetDefaultIngress(c.Request().Context(), middleware.Principal(c), c.FormValue("node"), c.FormValue("confirm"))
		return "Default ingress saved.", nil, err
	}), m)
	g.POST(nodesBase+"/default-exit", h.act("nodes", func(c echo.Context) (string, *service.Job, error) {
		_, err := h.orch.SetDefaultExit(c.Request().Context(), middleware.Principal(c), c.FormValue("node"))
		return "Default exit saved.", nil, err
	}), m)
	g.POST(nodesBase+"/:node/rejoin", func(c echo.Context) error {
		out, err := h.orch.RejoinNode(c.Request().Context(), middleware.Principal(c), c.Param("node"))
		if err != nil {
			return h.nodeAnswer(c, "security", "", err)
		}
		c.Set("rejoin", out)
		return h.nodeAnswer(c, "security", "New join command below.", nil)
	}, m)
	g.POST(nodesBase+"/rotate", h.act("nodes", func(c echo.Context) (string, *service.Job, error) {
		err := h.orch.RotateNodeKeys(c.Request().Context(), middleware.Principal(c), "")
		return "Rotating every node's keys, one at a time. The security log shows each.", nil, err
	}), m)
	g.POST(nodesBase+"/rotate-every", h.act("nodes", func(c echo.Context) (string, *service.Job, error) {
		days, err := strconv.Atoi(c.FormValue("days"))
		if err != nil {
			return "", nil, errs.Invalidf("days", "a whole number of days")
		}
		if err := h.orch.SetRotateSchedule(c.Request().Context(), middleware.Principal(c), days); err != nil {
			return "", nil, err
		}
		if days == 0 {
			return "Scheduled key rotation is off.", nil, nil
		}
		return fmt.Sprintf("Keys rotate every %d days.", days), nil, nil
	}), m)
	g.POST(nodesBase+"/trust/rotate-root", h.act("nodes", func(c echo.Context) (string, *service.Job, error) {
		err := h.orch.RotateRoot(c.Request().Context(), middleware.Principal(c))
		return "Rotating the root. Every node gets a new certificate; the security log shows each step.", nil, err
	}), m)
	g.POST(nodesBase+"/trust/finish-rollover", h.act("nodes", func(c echo.Context) (string, *service.Job, error) {
		err := h.orch.FinishRollover(c.Request().Context(), middleware.Principal(c))
		return "Rollover finished. Nodes still on the old root need a rejoin.", nil, err
	}), m)
	g.POST(nodesBase+"/trust/revoke-controller-cert", h.act("nodes", func(c echo.Context) (string, *service.Job, error) {
		err := h.orch.RevokeControllerCert(c.Request().Context(), middleware.Principal(c))
		return "Replacing the controller certificate. Workers stop accepting the old one within minutes.", nil, err
	}), m)
	g.POST(nodesBase+"/trust/break-glass", h.act("nodes", func(c echo.Context) (string, *service.Job, error) {
		if c.FormValue("confirm") != breakGlassWord {
			return "", nil, errs.Invalidf("confirm", "type %q to confirm", breakGlassWord)
		}
		if c.FormValue("backup") != "" {
			_, err := h.orch.BackupEveryNode(c.Request().Context(), middleware.Principal(c))
			return "Backups queued; break glass once they finish.", nil, err
		}
		err := h.orch.BreakGlass(c.Request().Context(), middleware.Principal(c))
		return "Break glass done. Get a rejoin command for each node below.", nil, err
	}), m)
	g.POST(nodesBase+"/:node/trust-rejoin", func(c echo.Context) error {
		out, err := h.orch.RejoinNode(c.Request().Context(), middleware.Principal(c), c.Param("node"))
		if err != nil {
			return h.answer(c, "nodes", "", extra{}, err)
		}
		c.Set("rejoin", out)
		return h.answer(c, "nodes", "New join command below.", extra{}, nil)
	}, m)
	g.POST(nodesBase+"/:node/rotate", func(c echo.Context) error {
		err := h.orch.RotateNodeKeys(c.Request().Context(), middleware.Principal(c), c.Param("node"))
		return h.nodeAnswer(c, "security", "Rotating keys. The security log shows when it is done.", err)
	}, m)
	g.POST(nodesBase+"/:node/remove", h.nodeJob("security", "Node removal queued.", func(c echo.Context) (service.Job, error) {
		return h.orch.RemoveNode(c.Request().Context(), middleware.Principal(c), c.Param("node"))
	}), m)
	g.POST(nodesBase+"/:node/backup", h.nodeJob("overview", "Node backup queued.", func(c echo.Context) (service.Job, error) {
		return h.orch.BackupNode(c.Request().Context(), middleware.Principal(c), c.Param("node"))
	}), m)
	// An offline node is forgotten at once: the Nodes tab. One still online
	// is cleaned by a job first: its drawer, with the job.
	g.POST(nodesBase+"/:node/forget", func(c echo.Context) error {
		j, err := h.orch.ForgetNode(c.Request().Context(), middleware.Principal(c), c.Param("node"), c.FormValue("confirm"))
		switch {
		case err != nil:
			return h.nodeAnswer(c, "security", "", err)
		case j.ID != "":
			c.Set("job", j)
			return h.nodeAnswer(c, "security", "Cleaning the box, then forgetting the node.", nil)
		}
		return h.answer(c, "nodes", "Node forgotten.", extra{}, nil)
	}, m)
}

// nodeJob runs an action that queues a job and answers the node drawer on a
// tab with the job's status; a refusal answers it with the banner.
func (h *handler) nodeJob(tab, note string, do func(echo.Context) (service.Job, error)) echo.HandlerFunc {
	return func(c echo.Context) error {
		j, err := do(c)
		if err != nil {
			return h.nodeAnswer(c, tab, "", err)
		}
		c.Set("job", j)
		return h.nodeAnswer(c, tab, note, nil)
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
	// the plan's "Add this node" link prefills the name and tags from the file
	var tags []string
	for _, t := range strings.Split(c.QueryParam("tags"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			tags = append(tags, t)
		}
	}
	v, err := h.step1(c, strings.TrimSpace(c.QueryParam("name")), tags, "")
	if err != nil {
		return middleware.HTTPError(err)
	}
	return h.answer(c, "nodes", "", extra{adder: v}, nil)
}

func (h *handler) step1(c echo.Context, name string, picked []string, msg string) (*ui.AddNodeView, error) {
	return &ui.AddNodeView{
		Step: 1, Name: name, Picked: picked, Tags: service.NodeTags, Error: msg,
		Create: nodesBase + "/add", Cancel: ui.Base + "?tab=nodes",
	}, nil
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
		v, err := h.step1(c, name, f["tag"], msg)
		if err != nil {
			return middleware.HTTPError(err)
		}
		return h.answer(c, "nodes", "", extra{adder: v}, nil)
	}
	return h.answer(c, "nodes", "", extra{adder: &ui.AddNodeView{
		Step: 2, Name: out.NodeName, Command: out.OneLine, TwoStep: out.TwoStep,
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
	r := pendingRow(p)
	return respond.HTML(c, http.StatusOK, ui.AddNode(ui.AddNodeView{Step: 3, Name: p.Name, Pending: &r}))
}

// pendingRow is a machine waiting for a confirm, as the add steps and the
// Nodes tab show it. System is "" when it reported nothing.
func pendingRow(p service.PendingNode) ui.PendingRow {
	var sys []string
	if p.Facts.Kernel != "" {
		sys = append(sys, p.Facts.Kernel)
	}
	if p.Facts.CPUs > 0 {
		sys = append(sys, fmt.Sprintf("%d cores", p.Facts.CPUs))
	}
	if p.Facts.MemBytes > 0 {
		sys = append(sys, gb(p.Facts.MemBytes))
	}
	return ui.PendingRow{
		Name: p.Name, System: strings.Join(sys, " · "), Addresses: p.Addresses, Tags: p.Tags,
		Requested: ago(p.RequestedAt),
		Confirm:   nodesBase + "/pending/" + p.ID + "/confirm",
		Reject:    nodesBase + "/pending/" + p.ID + "/reject",
	}
}

func gb(b uint64) string { return fmt.Sprintf("%.0f GB", float64(b)/(1<<30)) }

func (h *handler) nodesTab(c echo.Context, x extra) (templ.Component, error) {
	ctx := c.Request().Context()
	ns, err := h.orch.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	lost, err := h.orch.LostEnvs(ctx)
	if err != nil {
		return nil, err
	}
	v := ui.NodesView{Add: nodesBase + "/add", Adder: x.adder, Placements: ui.Base + "?tab=placements"}
	ps, err := h.orch.PendingNodes(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range ps {
		// the open add steps already show the one they wait for
		if x.adder == nil || x.adder.Name != p.Name {
			v.Pending = append(v.Pending, pendingRow(p))
		}
	}
	for _, n := range ns {
		v.Rows = append(v.Rows, ui.NodeRow{
			Name: n.Name, Role: n.Role, Status: n.Status, Tags: n.Tags, Envs: n.Envs,
			Open: nodesBase + "/" + n.ID, Hint: ui.NodeHint(n.Status, n.Stalled),
		})
	}
	if len(ns) > 1 {
		d := &ui.DefaultsView{IngressAction: nodesBase + "/default-ingress/preview", Preview: x.ingress, ExitAction: nodesBase + "/default-exit",
			RotateAction: nodesBase + "/rotate", RotateEvery: nodesBase + "/rotate-every", RotateDays: h.orch.RotateSchedule(ctx)}
		if id, err := h.orch.DefaultIngress(ctx); err == nil {
			d.Ingress = id
		}
		d.Exit = h.orch.DefaultExit(ctx)
		for _, n := range ns {
			o := ui.Option{Value: n.ID, Label: n.Name}
			if slices.Contains(n.Tags, "ingress") {
				d.IngressNodes = append(d.IngressNodes, o)
			}
			if slices.Contains(n.Tags, "egress") {
				d.ExitNodes = append(d.ExitNodes, o)
			}
		}
		v.Defaults = d
	}
	for _, l := range lost {
		v.Lost = append(v.Lost, ui.LostEnvRow{Path: l.Path, Node: l.Node, Placement: l.Placement, Volumes: l.Volumes})
	}
	if v.Trust, err = h.trustView(c); err != nil {
		return nil, err
	}
	return ui.Nodes(v), nil
}

// POST /-/admin/nodes/default-ingress/preview: where each host that would
// enter by the node points now, then the typed-name confirm.
func (h *handler) previewIngress(c echo.Context) error {
	p, err := h.orch.IngressSwitchPlan(c.Request().Context(), middleware.Principal(c), c.FormValue("node"))
	if err != nil {
		return h.answer(c, "nodes", "", extra{}, err)
	}
	v := &ui.IngressPreview{Node: p.Node, Cancel: ui.Base + "?tab=nodes"}
	for _, ho := range p.Hosts {
		r := ui.IngressHostRow{Host: ho.Host, OK: ho.OK}
		switch {
		case ho.Error != "":
			r.Points = "lookup failed: " + ho.Error
		case ho.OK:
			r.Points = "points here"
		default:
			r.Points = "points to " + strings.Join(ho.Points, ", ")
		}
		v.Hosts = append(v.Hosts, r)
		if ho.Panel {
			v.PanelNote = "The panel moves to " + p.Node + "."
		}
	}
	vals, _ := json.Marshal(map[string]string{"node": p.Node, "confirm": p.Node})
	v.Confirm = comp.ConfirmView{
		Button: "Move default ingress", Title: "Move the default ingress to " + p.Node + "?",
		Warning: "The panel and every route without its own ingress enter by " + p.Node + ".",
		Kept:    []string{"apps running on every node"},
		Word:    p.Node, Action: nodesBase + "/default-ingress", Target: "#" + comp.DrawerRoot, Vals: string(vals),
	}
	return h.answer(c, "nodes", "", extra{ingress: v}, nil)
}

// trustView is the cluster trust section: the root, the buttons with what
// each one costs, and the nodes break glass left unjoined.
func (h *handler) trustView(c echo.Context) (ui.TrustView, error) {
	t, err := h.orch.ClusterTrust(c.Request().Context(), middleware.Principal(c))
	if err != nil {
		return ui.TrustView{}, err
	}
	day := func(t time.Time) string { return t.Local().Format("Jan 2, 2006") }
	v := ui.TrustView{
		Fingerprint: t.Fingerprint, Created: day(t.CreatedAt), Expires: day(t.ExpiresAt), RollingOver: t.RollingOver,
	}
	if t.RolloverStartedAt != nil {
		v.Since = day(*t.RolloverStartedAt)
	}
	if t.BreakGlassAt != nil {
		v.BreakGlassAt = day(*t.BreakGlassAt)
	}
	for _, n := range t.OnOldRoot {
		v.OnOldRoot = append(v.OnOldRoot, n.Name)
	}
	const act = nodesBase + "/trust/"
	v.Rotate = comp.ConfirmView{
		Button: "Rotate root", Title: "Rotate the root?", Quiet: true, Primary: true,
		Warning: "Every node gets a new certificate. Nothing stops while it runs.",
		Action:  act + "rotate-root", Target: "#" + comp.DrawerRoot,
	}
	v.Finish = comp.ConfirmView{
		Button: "Finish rollover now", Title: "Finish the rollover now?", Quiet: true,
		Warning: "The old root stops working. Nodes still on it need a rejoin.",
		Action:  act + "finish-rollover", Target: "#" + comp.DrawerRoot,
	}
	v.RevokeController = comp.ConfirmView{
		Button: "Revoke controller certificate", Title: "Revoke the controller certificate?", Quiet: true,
		Warning: "The controller gets a new certificate. Workers stop accepting the old one.",
		Action:  act + "revoke-controller-cert", Target: "#" + comp.DrawerRoot,
	}
	v.BreakGlass = comp.ConfirmView{
		Button: "Break glass", Title: "Break glass?",
		Warning: "Tunnels between nodes stop until each node rejoins. Apps keep running where they are. Deploys and traffic between nodes stop.",
		Kept:    []string{"apps running on every node", "node names, envs and addresses"},
		Word:    breakGlassWord, Action: act + "break-glass", Target: "#" + comp.DrawerRoot,
		Vals: `{"confirm":"` + breakGlassWord + `"}`,
		Body: ui.BreakGlassBody(), Include: "input[name=backup]",
	}
	cmd, _ := c.Get("rejoin").(service.JoinTokenOut)
	for _, n := range t.NotRejoined {
		r := ui.NotRejoinedRow{Name: n.Name, Rejoin: nodesBase + "/" + n.ID + "/trust-rejoin"}
		if cmd.Command != "" && c.Param("node") == n.ID {
			r.Command, r.TwoStep = cmd.OneLine, cmd.TwoStep
			r.Expires = "in " + fmt.Sprint(int(time.Until(cmd.ExpiresAt).Round(time.Minute).Minutes())) + " minutes"
		}
		v.NotRejoined = append(v.NotRejoined, r)
	}
	return v, nil
}

// GET /-/admin/nodes/:node?tab=
// GET …/nodes/:node/metrics/events: the Metrics tab's new points.
func (h *handler) nodeMetricsEvents(c echo.Context) error {
	s, err := h.orch.NodeScope(c.Request().Context(), c.Param("node"))
	if err != nil {
		return middleware.HTTPError(err)
	}
	return render.MetricsEvents(c, h.orch, s)
}

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
		Note: note,
	}
	// Opened from the cluster map, "back" has nowhere to go.
	if cur, err := url.Parse(c.Request().Header.Get("HX-Current-URL")); err != nil || !strings.HasPrefix(cur.Path, "/-/cluster") {
		f.Actions = ui.NodeBack(ui.Base + "?tab=nodes")
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
		ID: d.ID, Name: d.Name, Role: d.Role, Status: d.Status, Hint: ui.NodeHint(d.Status, d.Stalled), Controller: d.Controller, Tags: d.Tags,
		Range: d.Range, WGAddress: d.WGAddress, AllTags: service.NodeTags, TagsAction: base + "/tags",
	}
	ctx := c.Request().Context()
	if c.QueryParam("tab") == "metrics" {
		r := comp.MetricRange(c.QueryParam("range"))
		in := service.MetricsIn{}
		in.Range, _ = time.ParseDuration(r)
		s, err := h.orch.NodeMetrics(ctx, d.ID, in)
		if err != nil {
			return v, err
		}
		v.Metrics = render.MetricsLive(h.orch, s, r, base+"?tab=metrics", base+"/metrics/events")
		v.MemTotal, v.DiskTotal = lastBytes(s, "mem_total"), lastBytes(s, "disk_total")
	}
	live, err := h.orch.NodeLive(ctx, d.ID)
	if err != nil {
		return v, err
	}
	v.Behind, v.TooOld, v.CertExpired = live.Behind, live.TooOld, live.CertExpired
	for _, w := range live.Warnings {
		v.Warnings = append(v.Warnings, ui.WarningRow{Detail: w.Detail, Critical: w.Severity == service.SevCritical})
	}
	if j, ok := c.Get("job").(service.Job); ok {
		jv := render.JobView("", j)
		v.Job = &jv
	}
	if d.Status == "online" {
		v.Backup = base + "/backup"
	}
	gs, err := h.orch.NodeBackups(ctx, d.ID)
	if err != nil {
		return v, err
	}
	for _, g := range gs {
		r := ui.BackupGroupRow{Label: g.Label, Status: g.Status, Dest: g.Dest, Warning: g.Warning, Error: g.Error}
		for _, bv := range g.Volumes {
			r.Volumes = append(r.Volumes, ui.BackupVolumeRow{
				Volume: bv.Volume, Where: bv.Where, Method: bv.Method, Status: bv.Status, Error: bv.Error,
				Size: render.Size(bv.SizeBytes),
			})
		}
		v.Backups = append(v.Backups, r)
	}
	if f := live.Facts; f != nil {
		v.HasFacts = true
		v.Arch, v.Version, v.Cores, v.Mem = f.Arch, f.Version, f.CPUs, gb(f.MemBytes)
		v.Kernel, v.Docker = f.Kernel, f.Docker
		v.Uptime = uptime(time.Duration(f.UptimeS) * time.Second)
		v.Disk = render.Size(int64(f.DiskFree)) + " free"
	}
	v.LastSeen = ago(live.LastSeen)
	for _, i := range live.Ifaces {
		v.Ifaces = append(v.Ifaces, ui.IfaceRow{Group: i.Group, Name: i.Name, Addr: i.Addr, Label: i.Label})
	}
	tuns, err := h.orch.NodeTunnels(ctx, d.ID)
	if err != nil {
		return v, err
	}
	for _, t := range tuns {
		v.Tunnels = append(v.Tunnels, tunnelRow(d.ID, t))
	}
	x, err := h.orch.NodeExits(c.Request().Context(), d.ID)
	if err != nil {
		return v, err
	}
	picks := x.Ladder
	if len(picks) == 0 && x.Suggested != "" {
		picks = []string{x.Suggested}
	}
	copy(v.Exits[:], picks)
	for _, o := range x.Options {
		v.ExitNodes = append(v.ExitNodes, ui.Option{Value: o.Name, Label: o.Name})
	}
	if d.Controller {
		v.RoutesNote = fmt.Sprintf("%s serves the panel and %d routes.", d.Name, d.RoutesServed)
	}
	for _, e := range d.EnvList {
		v.Envs = append(v.Envs, ui.NodeEnvRow{Path: e.Path, Pin: e.Pin, Tiles: e.Tiles})
	}
	var newest time.Time // the newest valid cert's expiry
	for _, ce := range d.Certs {
		st := "valid"
		switch {
		case ce.Revoked:
			st = "revoked"
		case time.Now().After(ce.NotAfter):
			st = "expired"
		case ce.NotAfter.After(newest):
			newest = ce.NotAfter
		}
		v.Certs = append(v.Certs, ui.CertRow{
			Serial: ce.Serial, State: st,
			Issued: ce.IssuedAt.Format("Jan 2, 2006"), Expires: ce.NotAfter.Format("Jan 2, 2006"),
			RenewFrom: ce.RenewFrom.Format("Jan 2"),
		})
	}
	if !newest.IsZero() {
		v.CertExpiry = newest.Format("Jan 2, 2006")
	}
	v.Rotate = base + "/rotate"
	if !d.Controller {
		v.Rejoin = base + "/rejoin"
		if out, ok := c.Get("rejoin").(service.JoinTokenOut); ok {
			v.JoinCommand, v.JoinTwoStep = out.OneLine, out.TwoStep
			v.JoinExpires = "in " + fmt.Sprint(int(time.Until(out.ExpiresAt).Round(time.Minute).Minutes())) + " minutes"
		}
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
			Warning: "Stops everything on " + d.Name + ", backs it up, checks the backups, then cleans the box and removes it. A failed backup stops here and starts the apps again.",
			Kept:    []string{"the backups, and the volumes and images on the box"},
			Action:  base + "/remove", Target: "#" + comp.DrawerRoot,
		}
		v.Forget = comp.ConfirmView{
			Button: "Forget node", Title: "Forget " + d.Name + "?",
			Warning: "Use this only if " + d.Name + " is gone for good. If it still answers, its containers, networks and tunnel come off first.",
			Kept:    []string{"its envs' rows and settings, listed as lost until you redeploy them", "the volumes on the box"},
			Word:    d.Name, Action: base + "/forget", Target: "#" + comp.DrawerRoot,
			Vals: string(vals),
			Body: ui.ForgetWarning(fb),
		}
	}
	return v, nil
}

// tunnelRow is one peer of the node: who dials, the ping, and the tunnel
// drawer that opens in place.
func tunnelRow(self string, t service.NodeTunnel) ui.TunnelRow {
	l := t.Link
	me, peer := l.Ends[0], l.Ends[1]
	if l.B == self {
		me, peer = peer, me
	}
	r := ui.TunnelRow{Name: t.PeerName, Open: clusterui.TunnelDrawer(self, t.Peer)}
	switch {
	case l.Via != "":
		r.Dials = "via " + l.Via
	case me.Dials && peer.Dials:
		r.Dials = "both dial"
	case me.Dials:
		r.Dials = "this node dials"
	case peer.Dials:
		r.Dials = t.PeerName + " dials"
	}
	if l.MS >= 0 {
		r.Ping = strconv.FormatFloat(l.MS, 'f', -1, 64) + " ms"
	}
	return r
}

// ago reads "just now", "5 min ago", "3 h ago", "2 days ago"; zero = never.
func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case t.IsZero():
		return "not seen yet"
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d/time.Hour))
	}
	return fmt.Sprintf("%d days ago", int(d/(24*time.Hour)))
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// uptime reads "11 days", "3 h 20 min", "5 min".
func uptime(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d days", int(d/(24*time.Hour)))
	case d >= time.Hour:
		return fmt.Sprintf("%d h %d min", int(d/time.Hour), int(d%time.Hour/time.Minute))
	}
	return fmt.Sprintf("%d min", int(d/time.Minute))
}

// lastBytes is measure m's newest value in s as bytes, "" when it has none.
func lastBytes(s service.Series, m string) string {
	xs := s[m]
	if len(xs) == 0 {
		return ""
	}
	return service.HumanBytes(int64(xs[len(xs)-1].V))
}
