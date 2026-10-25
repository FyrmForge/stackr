package connpanel

import (
	"errors"
	"log/slog"
	"net/http"
	"slices"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	"github.com/FyrmForge/stackr/internal/ui/connectors"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// The Git source capability's tab: where its provider delivers events,
// the provider's page for it, who may use a server one, and the repos it
// may read. The same for every git kind.

func (p Panel) mountGit(g *echo.Group, prefix string, gate, repos echo.MiddlewareFunc) {
	g.POST(prefix+"/:connector/share", p.share, gate)
	g.POST(prefix+"/:connector/repos", p.repos, repos)
	g.POST(prefix+"/:connector/installed", p.installed, gate)
}

// shares is the server's git connections by id, with who they are shared
// with; none at an org level.
func (p Panel) shares(c echo.Context) (map[string]service.ServerConnector, error) {
	if p.Org(c) != nil {
		return nil, nil
	}
	cs, err := p.Orch.ServerConnectors(c.Request().Context())
	out := map[string]service.ServerConnector{}
	for _, k := range cs {
		out[k.ID] = k
	}
	return out, err
}

// sharedWith is a server connection's sharing in a few words.
func sharedWith(k service.ServerConnector) string {
	return comp.ShareSummary(k.ShareAll, len(k.OrgIDs))
}

// gitRow fills a git connection's row: at the server its sharing, where
// its provider delivers events and its install page; a connected one gets
// its install status as the row loads. At a level with a Drawer the row
// opens that instead of the dialog.
func (p Panel) gitRow(c echo.Context, k service.Connector, shares map[string]service.ServerConnector, r *connectors.Row) error {
	if p.Drawer != nil {
		r.Settings, r.Drawer, r.Push = "", p.Drawer(c, k), "?drawer=connector:"+k.ID
	}
	if sc, ok := shares[k.ID]; ok {
		r.Summary = sharedWith(sc)
		r.Detail = "Webhook URL " + p.Orch.ConnectorHookURL(c.Request().Context(), k.ID)
	}
	if k.Pending {
		return nil
	}
	r.Check = p.Base(c) + "/" + k.ID + "/installed"
	if p.Org(c) == nil {
		u, err := p.Orch.ConnectorInstallBack(c.Request().Context(), nil, k.ID, p.ret(c))
		if err != nil {
			return err
		}
		r.Link, r.LinkLabel = u, "Install on "+p.label(k)
	}
	return nil
}

// gitView is k's Git tab.
func (p Panel) gitView(c echo.Context, k service.Connector) (connectors.GitView, error) {
	ctx, org := c.Request().Context(), p.Org(c)
	v := connectors.GitView{ID: k.ID, Label: p.label(k), HookURL: p.Orch.ConnectorHookURL(ctx, k.ID)}
	var err error
	if v.Install, err = p.Orch.ConnectorInstallBack(ctx, org, k.ID, p.ret(c)); err != nil {
		return v, err
	}
	if !k.Pending {
		v.Repos = p.Base(c) + "/" + k.ID + "/repos"
	}
	if org != nil {
		return v, nil
	}
	shares, err := p.shares(c)
	if err != nil {
		return v, err
	}
	ogs, err := p.Orch.AllOrgs(ctx)
	if err != nil {
		return v, err
	}
	sc := shares[k.ID]
	sv := connectors.ShareView{
		ID: "share-" + k.ID, Action: p.Base(c) + "/" + k.ID + "/share", Target: "#connector-git-" + k.ID, All: sc.ShareAll,
	}
	for _, og := range ogs {
		sv.Orgs = append(sv.Orgs, connectors.ShareOrg{ID: og.ID, Name: og.Name, On: slices.Contains(sc.OrgIDs, og.ID)})
	}
	v.Share = &sv
	return v, nil
}

// POST <base>/:connector/share (scope=all|only, org...): who may use a
// server git connection; the tab again with the answer.
func (p Panel) share(c echo.Context) error {
	form, err := c.FormParams()
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad form")
	}
	ctx, id := c.Request().Context(), c.Param("connector")
	all := form.Get("scope") == "all" || form.Get("all") != ""
	orgs := form["org"]
	if all {
		orgs = nil
	}
	_, actErr := p.Orch.ShareConnector(ctx, id, orgs, all)
	k, err := p.Orch.ConnectorAt(ctx, p.Org(c), id)
	if err != nil {
		return middleware.HTTPError(err)
	}
	v, err := p.gitView(c, k)
	if err != nil {
		return middleware.HTTPError(err)
	}
	status := http.StatusOK
	if actErr != nil {
		msg, ok := render.Refusal(actErr)
		if !ok {
			return middleware.HTTPError(actErr)
		}
		v.Error, status = msg, http.StatusUnprocessableEntity
	} else {
		v.Note = "Saved."
	}
	return respond.HTML(c, status, connectors.Git(v))
}

// repos asks the provider what connection id may read: none for one
// installed nowhere; a provider that does not answer is the list's text.
func (p Panel) reposOf(c echo.Context) ([]service.Repo, error) {
	ctx, id := c.Request().Context(), c.Param("connector")
	if org := p.Org(c); org != nil {
		return p.Orch.ConnectorRepos(ctx, *org, id)
	}
	return p.Orch.ServerConnectorRepos(ctx, id)
}

// POST <base>/:connector/repos: the Git tab's repo list.
func (p Panel) repos(c echo.Context) error {
	rs, err := p.reposOf(c)
	if errors.Is(err, errs.ErrNotFound) {
		return middleware.HTTPError(err)
	}
	if err != nil {
		msg, ok := render.Refusal(err)
		if !ok {
			msg = "The provider did not answer."
		}
		return respond.HTML(c, http.StatusOK, connectors.Repos(nil, msg))
	}
	names := make([]string, len(rs))
	for i, r := range rs {
		names[i] = r.FullName
	}
	return respond.HTML(c, http.StatusOK, connectors.Repos(names, ""))
}

// POST <base>/:connector/installed: a row's install status, asked of the
// provider as the row loads. One that does not answer is a chip, not an
// error page: the rest of the tab still works.
func (p Panel) installed(c echo.Context) error {
	rs, err := p.reposOf(c)
	if errors.Is(err, errs.ErrNotFound) {
		return middleware.HTTPError(err)
	}
	if err != nil {
		slog.Warn("connector install check", "connector", c.Param("connector"), "err", err)
	}
	return respond.HTML(c, http.StatusOK, connectors.InstallStatus(len(rs), err != nil))
}
