package setup

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	hamrmw "github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	"github.com/FyrmForge/stackr/internal/ui/connectors"
	"github.com/FyrmForge/stackr/internal/ui/drawer/admin"
	pages "github.com/FyrmForge/stackr/internal/ui/pages/setup"
	"github.com/FyrmForge/stackr/internal/web/dnspanel"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// The server's first-run wizard under /-/setup/server: an admin's, before
// org setup. ServerPending sends an admin here while server_setup_done_at is
// unset; there is no restore branch (restore is docs only).

// ServerBase is where the wizard lives.
const ServerBase = "/-/setup/server"

// MountServer registers the wizard. Mount the group after Load.
func (h *handler) MountServer(site *echo.Group, a *middleware.Access) {
	page, bind := a.LoginFirst(), a.Require("serverconfig.bind")
	site.GET(ServerBase, h.serverStart, page, bind)
	site.GET(ServerBase+"/ui", h.serverHand, page, bind)
	site.GET(ServerBase+"/ui/:step", h.serverStep, page, bind)
	site.GET(ServerBase+"/config", h.serverFile, page, bind)
	site.GET(ServerBase+"/config/plan", h.serverPlan, page, bind)
	site.POST(ServerBase+"/mode", h.serverMode, bind)
	set := a.Require("serverdefaults.set")
	site.POST(ServerBase+"/domains", h.serverDomains, set)
	site.POST(ServerBase+"/github", h.serverGitHub, a.Require("connector.admin"))
	site.POST(ServerBase+"/github/app", h.serverApp, a.Require("connector.admin"))
	site.GET(dnsBase+"/new", func(c echo.Context) error {
		return respond.HTML(c, http.StatusOK, h.dnsPanel().Open(c))
	}, a.Require("connector.admin"))
	h.dnsPanel().Mount(site, dnsBase, a.Require("connector.admin"))
	site.POST(ServerBase+"/observe", h.serverObserve, set)
	site.POST(ServerBase+"/dest", h.serverDest, set)
	site.POST(ServerBase+"/panel-backup", h.serverPanelBackup, set)
	site.POST(ServerBase+"/bind", h.serverBind, bind)
	node := a.Require("node.manage")
	site.GET(nodesBase, h.serverNodes, node)
	site.POST(nodesBase+"/add", h.serverMintNode, node)
	site.GET(nodesBase+"/add/poll", h.serverPollNode, node)
	site.POST(nodesBase+"/pending/:node/confirm", h.serverJoin(true), node)
	site.POST(nodesBase+"/pending/:node/reject", h.serverJoin(false), node)
	approve := a.Require("serverplan.approve")
	site.POST(ServerBase+"/config/plan/:plan/approve", h.serverApprove, approve)
	site.POST(ServerBase+"/config/plan/:plan/reject", h.serverReject, approve)
	site.POST(ServerBase+"/done", h.serverDone, bind)
}

// ServerPending sends an admin's page requests to the wizard until it is
// finished. Anyone else, an htmx or API call, a write, the wizard's own
// paths and GitHub's returns from the wizard's App flow pass. Mount after
// Load, before the org gate.
func ServerPending(orch *service.Orchestrator) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			r := c.Request()
			p := middleware.Principal(c)
			if p == nil || !p.Access.Admin || r.Method != http.MethodGet || isHTMX(c) ||
				strings.HasPrefix(r.URL.Path, ServerBase) || strings.HasPrefix(r.URL.Path, "/api/") ||
				strings.HasPrefix(r.URL.Path, "/settings/github/") ||
				strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/logout" {
				return next(c)
			}
			s, err := orch.ServerSetupState(r.Context())
			if err != nil {
				return middleware.HTTPError(err)
			}
			if s.Done {
				return next(c)
			}
			return c.Redirect(http.StatusSeeOther, ServerBase)
		}
	}
}

// serverFrame is a titled shell with no stepper: the branches have two or three screens.
func serverFrame(title, subtitle string) pages.Frame {
	return pages.Frame{Title: title, Subtitle: subtitle}
}

// GET /-/setup/server: the branch question, or the branch already picked.
func (h *handler) serverStart(c echo.Context) error {
	s, err := h.orch.ServerSetupState(c.Request().Context())
	if err != nil {
		return middleware.HTTPError(err)
	}
	pick := c.QueryParam("pick") != ""
	// a finished server still gets the branch question when asked for it:
	// the Setup guide's domains screen links here to switch to a file
	if s.Done && !pick {
		return respond.Redirect(c, "/")
	}
	if s.Mode != "" && !pick {
		return respond.Redirect(c, ServerBase+"/"+s.Mode)
	}
	return render.Bare(c, http.StatusOK, "Setup: This server", pages.ServerBranch(pages.ServerBranchView{Base: ServerBase}))
}

// POST /-/setup/server/mode (mode): the branch.
func (h *handler) serverMode(c echo.Context) error {
	s, err := h.orch.SetServerSetupMode(c.Request().Context(), middleware.Principal(c), c.FormValue("mode"))
	if err != nil {
		return back(c, err, ServerBase+"?pick=1")
	}
	return respond.Redirect(c, ServerBase+"/"+s.Mode)
}

// GET /-/setup/server/ui: the by-hand branch starts on its first screen.
func (h *handler) serverHand(c echo.Context) error {
	return respond.Redirect(c, stepURL(service.ServerSetupSteps[0]))
}

func stepURL(step string) string { return ServerBase + "/ui/" + step }

// adminURL is an Admin drawer tab, opened over the home page.
func adminURL(tab string) string { return "/?drawer=admin&tab=" + tab }

// serverSteps' titles and subtitles, by step.
var serverStepTitles = map[string][2]string{
	"domains":       {"Domains", "Where the panel and your stacks are reached."},
	"certificates":  {"Certificates", "HTTPS for every domain, wildcards where stackr runs the DNS."},
	"github":        {"GitHub", "One App for the whole server, so organizations need none of their own."},
	"observability": {"Observability", "Graphs and logs, kept past today."},
	"backups":       {"Backups", "Where backups go, and the panel's own."},
	"nodes":         {"Nodes", "The boxes stacks run on."},
	"summary":       {"Summary", "What is set up. Change any of it now or later."},
}

// GET /-/setup/server/ui/:step: one by-hand screen. A finished server still
// gets it: Admin's Setup guide links here.
func (h *handler) serverStep(c echo.Context) error { return h.showServerStep(c, c.Param("step")) }

func (h *handler) showServerStep(c echo.Context, step string) error {
	ctx := c.Request().Context()
	steps := service.ServerSetupSteps
	i := slices.Index(steps, step)
	if i < 0 {
		return echo.ErrNotFound
	}
	t := serverStepTitles[step]
	s := pages.ServerStep{
		Frame: pages.Frame{Title: t[0], Subtitle: t[1], Step: i + 1, Steps: serverSteps()},
		Base:  ServerBase,
	}
	if i+1 < len(steps) {
		s.Next = stepURL(steps[i+1])
	}
	st, err := h.orch.ServerSetupState(ctx)
	if err != nil {
		return middleware.HTTPError(err)
	}
	var body templ.Component
	switch step {
	case "domains":
		v := pages.ServerDomainsView{ServerStep: s}
		err = h.settings(ctx, map[string]*string{"root_domain": &v.Root, "panel_domain": &v.Panel, "acme_email": &v.Email})
		body = pages.ServerDomains(v)
	case "certificates":
		v := pages.ServerCertsView{ServerStep: s}
		v.List = connectors.ListView{
			Title: "DNS connectors", Scope: "instance",
			Help:     "Each managed domain is available to every organization. Admin, Connectors narrows that.",
			Add:      dnsBase + "/new",
			AddLabel: "+ Add Namecheap",
		}
		v.List.Rows, err = h.dnsPanel().Rows(c)
		v.Done = len(v.List.Rows) > 0
		body = pages.ServerCertificates(v)
	case "github":
		var v pages.ServerGitHubView
		v, err = h.githubStep(c, s)
		body = pages.ServerGitHub(v)
	case "observability":
		var o service.ObserveStatus
		o, err = h.orch.ObserveStatus(ctx)
		body = pages.ServerObserve(pages.ServerObserveView{
			ServerStep: s,
			Enabled:    o.Enabled,
			Retention:  strconv.Itoa(o.RetentionDays),
			MinFree:    strconv.FormatUint(o.MinFreeBytes>>30, 10),
			LogsDays:   strconv.Itoa(o.LogsDays),
			LogsMax:    strconv.FormatUint(o.LogsMaxBytes>>30, 10),
		})
	case "backups":
		var v pages.ServerBackupsView
		v, err = h.backupsStep(ctx, s, c.QueryParam("dest"))
		body = pages.ServerBackups(v)
	case "nodes":
		var v pages.ServerNodesView
		v, err = h.nodesStep(ctx, s, st.Done)
		body = pages.ServerNodes(v)
	case "summary":
		var ls []service.SetupLine
		ls, err = h.orch.ServerSetupSummary(ctx)
		v := pages.ServerSummaryView{ServerStep: s}
		for _, l := range ls {
			// Admin is behind the gate until Finish; the step itself is not.
			href := stepURL(l.Step)
			if st.Done {
				href = adminURL(summaryTab[l.Step])
			}
			v.Rows = append(v.Rows, pages.SummaryRow{Label: serverStepTitles[l.Step][0], State: l.State, Detail: l.Detail, Href: href})
		}
		body = pages.ServerSummary(v)
	}
	if err != nil {
		return middleware.HTTPError(err)
	}
	return render.Bare(c, status(c), "Setup: "+t[0], body)
}

// serverSteps is the by-hand branch's stepper; any done step reopens.
func serverSteps() []pages.StepLink {
	var out []pages.StepLink
	for _, s := range service.ServerSetupSteps {
		out = append(out, pages.StepLink{Name: serverStepTitles[s][0], Href: stepURL(s)})
	}
	return out
}

// refillStep answers a refused save with its by-hand step, as posted.
func (h *handler) refillStep(c echo.Context, err error, step string) error {
	return refill(c, err, func() error { return h.showServerStep(c, step) })
}

// summaryTab is the Admin tab each summary line lives on.
var summaryTab = map[string]string{
	"domains": "settings", "certificates": "connectors", "github": "connectors",
	"observability": "observability", "backups": "backups", "nodes": "nodes",
}

func (h *handler) settings(ctx context.Context, keys map[string]*string) error {
	for key, dst := range keys {
		val, err := h.orch.Setting(ctx, key)
		if err != nil {
			return err
		}
		*dst = val
	}
	return nil
}

func (h *handler) githubStep(c echo.Context, s pages.ServerStep) (pages.ServerGitHubView, error) {
	ctx := c.Request().Context()
	v := pages.ServerGitHubView{ServerStep: s, CSRF: render.Shell(c, "").CSRF}
	cs, err := h.orch.ServerConnectors(ctx)
	if err != nil {
		return v, err
	}
	for _, k := range cs {
		install, err := h.orch.ServerConnectorInstallURL(ctx, k.ID)
		if err != nil {
			return v, err
		}
		v.Apps = append(v.Apps, pages.ServerApp{Name: k.Name, HookURL: h.orch.ConnectorHookURL(k.ID), InstallURL: install})
		// one GitHub has not confirmed yet is no App: Skip stays Skip
		v.Done = v.Done || install != ""
	}
	return v, nil
}

// backupsStep: added is the destination just added, which the panel backup
// form picks while the panel still backs up to this box.
func (h *handler) backupsStep(ctx context.Context, s pages.ServerStep, added string) (pages.ServerBackupsView, error) {
	v := pages.ServerBackupsView{ServerStep: s}
	var on, dest string
	if err := h.settings(ctx, map[string]*string{"panel_backup_enabled": &on, "panel_backup_schedule": &v.Schedule, "panel_backup_dest": &dest}); err != nil {
		return v, err
	}
	if dest == "" {
		dest = added
	}
	v.PanelOn = on == "true"
	ds, err := h.orch.GlobalBackupDests(ctx)
	if err != nil {
		return v, err
	}
	for _, d := range ds {
		if d.Kind == "local" {
			continue
		}
		v.Dests = append(v.Dests, pages.ServerDest{Name: d.Name, Detail: d.Bucket})
		v.PanelDests = append(v.PanelDests, pages.Option{Value: d.ID, Label: d.Name, Picked: d.ID == dest})
	}
	return v, nil
}

func (h *handler) nodesStep(ctx context.Context, s pages.ServerStep, done bool) (pages.ServerNodesView, error) {
	v := pages.ServerNodesView{ServerStep: s}
	if done {
		v.AdminURL = adminURL("nodes")
	}
	var err error
	if v.Panel, err = h.nodesPanel(ctx, nil, ""); err != nil {
		return v, err
	}
	ps, err := h.orch.Placements(ctx)
	if err != nil {
		return v, err
	}
	for _, p := range ps {
		line := p.Name + ": "
		if p.NodeName == "" {
			line += "no node yet"
		} else {
			line += p.NodeName
		}
		v.Placements = append(v.Placements, line)
	}
	return v, nil
}

const nodesBase = ServerBase + "/nodes"

// nodesPanel is the Nodes step's live part; adder is the open add-node
// steps (nil = closed). skip is the machine they wait for, which the list
// leaves to them.
func (h *handler) nodesPanel(ctx context.Context, adder *admin.AddNodeView, skip string) (pages.NodesPanel, error) {
	v := pages.NodesPanel{Base: ServerBase, Adder: adder, Poll: nodesBase}
	if skip != "" {
		v.Poll += "?skip=" + url.QueryEscape(skip)
	}
	ns, err := h.orch.Nodes(ctx)
	if err != nil {
		return v, err
	}
	for _, n := range ns {
		v.Nodes = append(v.Nodes, pages.NodeLine{Name: n.Name, Role: n.Role, Status: admin.StatusWord(n.Status), Hint: admin.NodeHint(n.Status, n.Stalled)})
	}
	ps, err := h.orch.PendingNodes(ctx)
	if err != nil {
		return v, err
	}
	for _, p := range ps {
		if p.Name != skip {
			v.Pending = append(v.Pending, pendingRow(p))
		}
	}
	return v, nil
}

// pendingRow is a machine that called in, with the step's own confirm and
// reject.
func pendingRow(p service.PendingNode) admin.PendingRow {
	return admin.PendingRow{
		Name: p.Name, Addresses: p.Addresses, Tags: p.Tags,
		Confirm: nodesBase + "/pending/" + p.ID + "/confirm",
		Reject:  nodesBase + "/pending/" + p.ID + "/reject",
	}
}

// adderStep1 is Admin's first add-node step, answering into the panel.
func adderStep1(name string, picked []string, msg string) *admin.AddNodeView {
	return &admin.AddNodeView{
		Step: 1, Name: name, Picked: picked, Tags: service.NodeTags, Error: msg,
		Create: nodesBase + "/add", Cancel: nodesBase, Target: "#" + pages.NodesPanelID,
	}
}

// answerNodes is the panel, the add-node steps as adder leaves them.
func (h *handler) answerNodes(c echo.Context, adder *admin.AddNodeView, skip string) error {
	v, err := h.nodesPanel(c.Request().Context(), adder, skip)
	if err != nil {
		return middleware.HTTPError(err)
	}
	return respond.HTML(c, http.StatusOK, pages.ServerNodesPanel(v))
}

// GET /-/setup/server/nodes (add=1 opens the add-node steps; skip= is the
// machine open steps wait for): the panel, which the list polls.
func (h *handler) serverNodes(c echo.Context) error {
	var adder *admin.AddNodeView
	if c.QueryParam("add") != "" {
		adder = adderStep1("", nil, "")
	}
	return h.answerNodes(c, adder, c.QueryParam("skip"))
}

// POST /-/setup/server/nodes/add (name, tag): Admin's mint, step two in
// the panel.
func (h *handler) serverMintNode(c echo.Context) error {
	f, err := c.FormParams()
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad form")
	}
	name := strings.TrimSpace(f.Get("name"))
	out, err := h.orch.CreateJoinToken(c.Request().Context(), middleware.Principal(c), service.JoinTokenSpec{NodeName: name, Tags: f["tag"]})
	if err != nil {
		msg, ok := render.Refused(err)
		if !ok {
			return middleware.HTTPError(err)
		}
		return h.answerNodes(c, adderStep1(name, f["tag"], msg), "")
	}
	return h.answerNodes(c, &admin.AddNodeView{
		Step: 2, Name: out.NodeName, Command: out.OneLine, TwoStep: out.TwoStep,
		Expires: "in " + strconv.Itoa(int(time.Until(out.ExpiresAt).Round(time.Minute).Minutes())) + " minutes",
		Poll:    nodesBase + "/add/poll?name=" + url.QueryEscape(out.NodeName),
		Cancel:  nodesBase, Target: "#" + pages.NodesPanelID,
	}, out.NodeName)
}

// GET /-/setup/server/nodes/add/poll (name): 204 until the machine calls
// in, then step three.
func (h *handler) serverPollNode(c echo.Context) error {
	p, err := h.orch.PendingNodeByName(c.Request().Context(), c.QueryParam("name"))
	if errors.Is(err, errs.ErrNotFound) {
		return c.NoContent(http.StatusNoContent)
	}
	if err != nil {
		return middleware.HTTPError(err)
	}
	r := pendingRow(p)
	return respond.HTML(c, http.StatusOK, admin.AddNode(admin.AddNodeView{Step: 3, Name: p.Name, Pending: &r, Target: "#" + pages.NodesPanelID}))
}

// POST /-/setup/server/nodes/pending/:node/confirm and …/reject: the
// panel again, the steps closed; a refusal shows over a reopened step one.
func (h *handler) serverJoin(confirm bool) echo.HandlerFunc {
	return func(c echo.Context) error {
		ctx, by, id := c.Request().Context(), middleware.Principal(c), c.Param("node")
		var err error
		if confirm {
			_, err = h.orch.ConfirmJoin(ctx, by, id)
		} else {
			err = h.orch.RejectJoin(ctx, by, id)
		}
		if err != nil {
			msg, ok := render.Refused(err)
			if !ok {
				return middleware.HTTPError(err)
			}
			return h.answerNodes(c, adderStep1("", nil, msg), "")
		}
		return h.answerNodes(c, nil, "")
	}
}

// dnsBase is where the certificates screen's DNS connector actions hang.
const dnsBase = ServerBase + "/dns"

// dnsPanel is Admin's server DNS connectors on the certificates screen,
// its add dialog opened straight on Namecheap. Removing one is Admin's:
// the screen has no drawer to answer into.
func (h *handler) dnsPanel() dnspanel.Panel {
	return dnspanel.Panel{
		Orch:    h.orch,
		Org:     func(echo.Context) *string { return nil },
		Base:    func(echo.Context) string { return dnsBase },
		Delete:  func(echo.Context, service.Connector) comp.ConfirmView { return comp.ConfirmView{} },
		Refresh: func(echo.Context) string { return stepURL("certificates") },
	}
}

// POST /-/setup/server/domains (root_domain, panel_domain, acme_email).
func (h *handler) serverDomains(c echo.Context) error {
	vals := map[string]string{}
	for _, k := range []string{"root_domain", "panel_domain", "acme_email"} {
		vals[k] = strings.TrimSpace(c.FormValue(k))
	}
	if err := h.orch.SetSettings(c.Request().Context(), vals); err != nil {
		return h.refillStep(c, err, "domains")
	}
	hamrmw.SetFlash(c, "Domains saved.", hamrmw.FlashSuccess)
	return respond.Redirect(c, stepURL("certificates"))
}

// POST /-/setup/server/github (gh_org): a server GitHub App shared with
// every org, by GitHub's manifest flow. A native form: the answer is the
// page that posts the manifest to GitHub. GitHub's setup_url brings the
// browser back to the GitHub step while setup is unfinished.
func (h *handler) serverGitHub(c echo.Context) error {
	_, action, manifest, err := h.orch.BeginServerConnector(
		c.Request().Context(), middleware.Principal(c).User.ID, strings.TrimSpace(c.FormValue("gh_org")),
	)
	if err != nil {
		return back(c, err, stepURL("github"))
	}
	return render.Bare(c, http.StatusOK, "Taking you to GitHub", pages.GitHubRedirect(action, manifest))
}

// POST /-/setup/server/github/app (app_id, slug, private_key,
// webhook_secret): an existing App, shared with every org. The service
// checks the key parses.
func (h *handler) serverApp(c echo.Context) error {
	f := c.FormValue
	id, err := strconv.ParseInt(strings.TrimSpace(f("app_id")), 10, 64)
	if err != nil {
		return h.refillStep(c, errs.Invalidf("app_id", "the App ID is the number on the App's page"), "github")
	}
	if _, err := h.orch.AddServerConnector(c.Request().Context(), service.GitHubApp{
		ID: id, Slug: f("slug"), PEM: f("private_key"), WebhookSecret: f("webhook_secret"),
	}); err != nil {
		return h.refillStep(c, err, "github")
	}
	hamrmw.SetFlash(c, "App saved. Point its webhook at the URL below.", hamrmw.FlashSuccess)
	return respond.Redirect(c, stepURL("github"))
}

// POST /-/setup/server/observe: the switch and the knobs shown, nothing else
// (the access log flags keep their values).
func (h *handler) serverObserve(c echo.Context) error {
	f, on := c.FormValue, "false"
	if f("observe_enabled") != "" {
		on = "true"
	}
	if err := h.orch.SetSettings(c.Request().Context(), map[string]string{
		"observe_enabled":           on,
		"observe_metrics_retention": f("observe_metrics_retention"),
		"observe_min_free":          f("observe_min_free"),
		"observe_logs_retention":    f("observe_logs_retention"),
		"observe_logs_max_size":     f("observe_logs_max_size"),
	}); err != nil {
		return h.refillStep(c, err, "observability")
	}
	hamrmw.SetFlash(c, "Saved.", hamrmw.FlashSuccess)
	return respond.Redirect(c, stepURL("backups"))
}

// POST /-/setup/server/dest: a global backup destination, offered to every
// org when shared is ticked (the form ticks it).
func (h *handler) serverDest(c echo.Context) error {
	f := c.FormValue
	d, err := h.orch.CreateBackupDest(c.Request().Context(), nil, service.BackupDestSpec{
		Name: f("dest_name"), Endpoint: f("endpoint"), Region: f("region"),
		Bucket: f("bucket"), AccessKey: f("access_key"), SecretKey: f("secret_key"),
		Shared: f("shared") != "",
	})
	if err != nil {
		// the form calls the destination's name dest_name
		if inv, ok := errs.IsInvalid(err); ok && inv.Field == "name" {
			inv.Field, err = "dest_name", inv
		}
		return h.refillStep(c, err, "backups")
	}
	hamrmw.SetFlash(c, "Destination added.", hamrmw.FlashSuccess)
	return respond.Redirect(c, stepURL("backups")+"?dest="+url.QueryEscape(d.ID))
}

// POST /-/setup/server/panel-backup: the panel's own backup switch,
// schedule and destination; how many are kept stays as it is.
func (h *handler) serverPanelBackup(c echo.Context) error {
	f, on := c.FormValue, "false"
	if f("panel_backup_enabled") != "" {
		on = "true"
	}
	if err := h.orch.SetSettings(c.Request().Context(), map[string]string{
		"panel_backup_enabled":  on,
		"panel_backup_schedule": strings.TrimSpace(f("panel_backup_schedule")),
		"panel_backup_dest":     f("panel_backup_dest"),
	}); err != nil {
		return h.refillStep(c, err, "backups")
	}
	hamrmw.SetFlash(c, "Saved.", hamrmw.FlashSuccess)
	return respond.Redirect(c, stepURL("nodes"))
}

// GET /-/setup/server/config: the file branch.
func (h *handler) serverFile(c echo.Context) error {
	ctx := c.Request().Context()
	b, err := h.orch.ServerConfigBinding(ctx)
	if err != nil {
		return middleware.HTTPError(err)
	}
	cs, err := h.orch.ServerConnectors(ctx)
	if err != nil {
		return middleware.HTTPError(err)
	}
	v := pages.ServerFileView{
		Frame:  serverFrame("Set up from a file", "Bind the repo that holds stackr-server.yml, then approve its plan."),
		Base:   ServerBase,
		Repo:   strings.TrimPrefix(b.Repo, "https://github.com/"),
		Branch: b.Branch, Path: b.Path, Bound: b.Repo != "",
	}
	for _, k := range cs {
		v.Connectors = append(v.Connectors, pages.Option{Value: k.ID, Label: k.Name, Picked: k.ID == b.ConnectorID})
	}
	if v.Bound {
		ps, err := h.orch.ServerPlans(ctx, 1)
		if err != nil {
			return middleware.HTTPError(err)
		}
		if len(ps) > 0 {
			v.PlanURL = ServerBase + "/config/plan"
		}
	}
	return render.Bare(c, status(c), "Setup: This server", pages.ServerFile(v))
}

// POST /-/setup/server/bind (connector_id, repo, branch, path): bind and plan.
func (h *handler) serverBind(c echo.Context) error {
	f := c.FormValue
	repo := strings.TrimSpace(f("repo"))
	if repo == "" {
		return refill(c, errs.Invalidf("repo", "a repository is required"), func() error { return h.serverFile(c) })
	}
	if _, err := h.orch.BindServerConfig(c.Request().Context(), f("connector_id"), repo, f("branch"), f("path"), false); err != nil {
		return refill(c, err, func() error { return h.serverFile(c) })
	}
	hamrmw.SetFlash(c, "Planned.", hamrmw.FlashSuccess)
	return respond.Redirect(c, ServerBase+"/config/plan")
}

// GET /-/setup/server/config/plan: the latest plan, on the org wizard's plan screen.
func (h *handler) serverPlan(c echo.Context) error {
	ctx := c.Request().Context()
	ps, err := h.orch.ServerPlans(ctx, 1)
	if err != nil {
		return middleware.HTTPError(err)
	}
	if len(ps) == 0 {
		return respond.Redirect(c, ServerBase+"/config")
	}
	pl := ps[0]
	if pl.Status == "applied" {
		return respond.Redirect(c, ServerBase+"/config")
	}
	v := pages.PlanView{
		Frame:   serverFrame("Review the plan", "This is what applying the server file does."),
		Summary: pl.Summary,
		Commit:  pl.Commit[:min(8, len(pl.Commit))],
		When:    pl.CreatedAt,
		Error:   pl.Error,
	}
	applying := pl.Status == "pending" && pl.DecidedAt != nil
	if applying {
		v.Work, v.Poll = "Applying.", ServerBase+"/config/plan"
	}
	var p service.ServerConfigPlan
	if pl.Plan != "" {
		if err := json.Unmarshal([]byte(pl.Plan), &p); err != nil {
			return middleware.HTTPError(err)
		}
		v.Parsed, v.Errors, v.Warnings = true, p.Blockers, p.Notes
		// gaps are filled on Admin > Config, past the wizard
		for _, g := range p.Gaps {
			what := "Server secret " + g.Key
			if g.Kind == "node" {
				what = "Node " + g.Key
			}
			v.Warnings = append(v.Warnings, what+" is missing: finish setup, then fill it in on Admin > Config.")
		}
		for _, ch := range p.Changes {
			v.Changes = append(v.Changes, pages.Change{Kind: "update", Env: "server", Tile: ch.Tile, Field: ch.Field, Old: ch.Old, New: ch.New, Note: cmpNote(ch.Impact, ch.Note)})
		}
	}
	if pl.Status == "pending" && !applying {
		if p.CanApprove() {
			v.Approve = ServerBase + "/config/plan/" + pl.ID + "/approve"
		}
		v.Reject = ServerBase + "/config/plan/" + pl.ID + "/reject"
		if p.Blocked() {
			v.Blocked = "This plan has errors. Fix them in the file and plan again."
		}
	}
	return render.Bare(c, http.StatusOK, "Setup: This server", pages.Plan(v))
}

func cmpNote(impact, note string) string {
	if impact != "" {
		return impact
	}
	return note
}

// POST …/config/plan/:plan/approve
func (h *handler) serverApprove(c echo.Context) error {
	vals, _ := c.FormParams()
	if _, err := h.orch.ApproveServerPlan(c.Request().Context(), c.Param("plan"), service.ApproveOpts{
		Ticked: vals["ticked"], Confirm: c.FormValue("confirm") != "",
	}); err != nil {
		return back(c, err, ServerBase+"/config/plan")
	}
	return respond.Redirect(c, ServerBase+"/config/plan")
}

// POST …/config/plan/:plan/reject
func (h *handler) serverReject(c echo.Context) error {
	if _, err := h.orch.RejectServerPlan(c.Request().Context(), c.Param("plan")); err != nil {
		return back(c, err, ServerBase+"/config/plan")
	}
	hamrmw.SetFlash(c, "Plan rejected.", hamrmw.FlashSuccess)
	return respond.Redirect(c, ServerBase+"/config")
}

// POST /-/setup/server/done: stamp it; org setup follows as today.
func (h *handler) serverDone(c echo.Context) error {
	if err := h.orch.FinishServerSetup(c.Request().Context(), middleware.Principal(c)); err != nil {
		return middleware.HTTPError(err)
	}
	return respond.Redirect(c, "/")
}
