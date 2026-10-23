package connpanel

import (
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/ui/connectors"
	dnsui "github.com/FyrmForge/stackr/internal/ui/dns"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// The settings dialog: the core's Connection and Used by tabs around each
// capability's own (DNS: Domains, Records; Git: Repos).

// tabs is connection k's tabs, in order: key and label.
func tabs(k service.Connector) [][2]string {
	out := [][2]string{{"connection", "Connection"}}
	if k.Pending {
		return out
	}
	if slices.Contains(k.Capabilities, service.CapDNS) {
		out = append(out, [2]string{"domains", "Domains"}, [2]string{"records", "Records"})
	}
	if slices.Contains(k.Capabilities, service.CapGit) {
		out = append(out, [2]string{"git", "Repos"})
	}
	return append(out, [2]string{"used", "Used by"})
}

// Settings (GET <base>/:connector/settings?tab=): the settings dialog on
// a tab; asked from inside it (a tab), only its body.
func (p Panel) Settings(c echo.Context) error {
	ctx := c.Request().Context()
	k, err := p.Orch.ConnectorStatus(ctx, p.Org(c), c.Param("connector"))
	if err != nil {
		return middleware.HTTPError(err)
	}
	ts := tabs(k)
	tab := c.QueryParam("tab")
	if !slices.ContainsFunc(ts, func(t [2]string) bool { return t[0] == tab }) {
		// opens on what the connection is for, its first capability's
		// tab; Connection when it has none
		tab = ts[0][0]
		if len(ts) > 2 {
			tab = ts[1][0]
		}
	}
	body, err := p.tab(c, k, tab)
	if err != nil {
		return middleware.HTTPError(err)
	}
	url := p.Base(c) + "/" + k.ID + "/settings?tab="
	v := connectors.SettingsView{Body: body}
	for _, t := range ts {
		v.Tabs = append(v.Tabs, connectors.TabLink{Label: t[1], URL: url + t[0], On: t[0] == tab})
	}
	if p.Delete != nil {
		v.Delete = p.Delete(c, k)
	}
	if c.QueryParam("tab") != "" && c.Request().Header.Get("HX-Target") == connectors.BodyID {
		return respond.HTML(c, http.StatusOK, connectors.Settings(v))
	}
	return respond.HTML(c, http.StatusOK, connectors.Modal(k.Name, p.Refresh(c), connectors.Settings(v)))
}

func (p Panel) tab(c echo.Context, k service.Connector, tab string) (templ.Component, error) {
	switch tab {
	case "domains":
		return dnsui.Load(k.ID, p.DNSBase(c)+"/"+k.ID+"/domains", p.label(k)), nil
	case "records":
		return p.records(c, k)
	case "git":
		v, err := p.gitView(c, k)
		return connectors.Git(v), err
	case "used":
		us, err := p.Orch.ConnectorUsages(c.Request().Context(), p.Org(c), k.ID)
		return connectors.Used(Usages(us)), err
	}
	v, err := p.connection(c, k, nil)
	return connectors.Connection(v), err
}

// connection is the Connection tab of k; vals, set, refill the credentials
// form after a refusal (never a secret).
func (p Panel) connection(c echo.Context, k service.Connector, vals map[string]string) (connectors.ConnectionView, error) {
	base := p.Base(c) + "/" + k.ID
	v := connectors.ConnectionView{ID: k.ID, Name: k.Name, Kind: p.label(k), Rename: base + "/rename"}
	if k.Pending {
		v.Pending = "Not finished at " + v.Kind + " yet. It goes away after an hour."
		v.Rename = ""
		return v, nil
	}
	v.Check = base + "/check"
	switch {
	case k.CheckedAt == nil:
		v.Status = "Not checked yet"
	case k.CheckError != "":
		v.Status, v.Tone = k.CheckError, "danger"
	default:
		v.Status, v.Tone = "Working, checked "+ago(*k.CheckedAt), "success"
	}
	fs, cur, err := p.Orch.ConnectorCredentials(c.Request().Context(), p.Org(c), k.ID)
	if err != nil {
		return v, err
	}
	if len(fs) > 0 {
		if vals == nil {
			vals = cur
		}
		v.Update = base + "/credentials"
		v.Fields = p.fieldsOf(c, fs, vals, nil)
	}
	return v, nil
}

// ago words how long ago t was.
func ago(t time.Time) string {
	d := time.Since(t).Round(time.Minute)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		if m := int(d.Minutes()); m == 1 {
			return "1 min ago"
		} else {
			return itoa(m) + " min ago"
		}
	case d < 48*time.Hour:
		return itoa(int(d.Hours())) + " h ago"
	}
	return t.Local().Format("Jan 2 15:04")
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}

// answerConn is the Connection tab again, an action's note or refusal on
// top; a provider's refusal is its text, not an error page.
func (p Panel) answerConn(c echo.Context, k service.Connector, note string, actErr error, vals map[string]string) error {
	if k.ID == "" {
		var err error
		if k, err = p.Orch.ConnectorStatus(c.Request().Context(), p.Org(c), c.Param("connector")); err != nil {
			return middleware.HTTPError(err)
		}
	}
	v, err := p.connection(c, k, vals)
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
		v.Note = note
	}
	return respond.HTML(c, status, connectors.Connection(v))
}

// POST <base>/:connector/check: Check now.
func (p Panel) check(c echo.Context) error {
	k, err := p.Orch.CheckConnector(c.Request().Context(), p.Org(c), c.Param("connector"))
	if err != nil {
		return middleware.HTTPError(err)
	}
	return p.answerConn(c, k, "", nil, nil)
}

// POST <base>/:connector/rename (name).
func (p Panel) rename(c echo.Context) error {
	_, err := p.Orch.RenameConnectorAt(c.Request().Context(), p.Org(c), c.Param("connector"), c.FormValue("name"))
	return p.answerConn(c, service.Connector{}, "Renamed.", err, nil)
}

// POST <base>/:connector/credentials (the kind's fields): checked, then
// saved; a refusal keeps the old ones and refills the form but a secret.
func (p Panel) update(c echo.Context) error {
	ctx, org, id := c.Request().Context(), p.Org(c), c.Param("connector")
	fs, _, err := p.Orch.ConnectorCredentials(ctx, org, id)
	if err != nil {
		return middleware.HTTPError(err)
	}
	vals := map[string]string{}
	for _, f := range fs {
		vals[f.Name] = c.FormValue(f.Name)
	}
	k, err := p.Orch.UpdateConnector(ctx, org, id, vals)
	if err != nil {
		for _, f := range fs {
			if f.Secret {
				delete(vals, f.Name)
			}
		}
		return p.answerConn(c, service.Connector{}, "", err, vals)
	}
	return p.answerConn(c, k, "Saved and checked.", nil, nil)
}

// Usages is us as rows, each path linked to its drawer.
func Usages(us []service.Usage) []connectors.UsageRow {
	out := make([]connectors.UsageRow, len(us))
	for i, u := range us {
		out[i] = connectors.UsageRow{Path: u.Path, What: u.What, Href: Href(u)}
	}
	return out
}

// Href is the drawer a usage opens: its level's page with the drawer.
func Href(u service.Usage) string {
	page := "/" + u.Org
	switch u.Level {
	case service.ScopeOrg:
		return page + "?drawer=org:" + u.ID
	case service.ScopeStack:
		return page + "/" + u.Stack + "?drawer=stack:" + u.ID
	case service.ScopeTile:
		return page + "/" + u.Stack + "/" + u.Env + "?drawer=tile:" + u.ID
	}
	return page
}

// InUse is err as a refusal listing where the thing is used, linked; nil
// when err is no InUse.
func InUse(err error) templ.Component {
	var in *service.InUse
	if !errors.As(err, &in) {
		return nil
	}
	return connectors.InUse(in.Msg+in.Tail, Usages(in.Usages))
}

// RemoveRefused answers a Remove refused because the connection is used:
// the usages, linked, on top of the settings dialog it was asked from.
// false: err is no such refusal, the level answers it.
func RemoveRefused(c echo.Context, err error) (bool, error) {
	in := InUse(err)
	if in == nil {
		return false, nil
	}
	c.Response().Header().Set("HX-Retarget", "#"+connectors.BodyID)
	c.Response().Header().Set("HX-Reswap", "afterbegin")
	return true, respond.HTML(c, http.StatusUnprocessableEntity, in)
}

// records is the Records tab: stackr's own records, by domain.
func (p Panel) records(c echo.Context, k service.Connector) (templ.Component, error) {
	rs, err := p.Orch.ConnectorRecords(c.Request().Context(), p.Org(c), k.ID)
	if err != nil {
		return nil, err
	}
	var groups []connectors.RecordGroup
	for _, r := range rs {
		if len(groups) == 0 || groups[len(groups)-1].Domain != r.Domain {
			groups = append(groups, connectors.RecordGroup{Domain: r.Domain})
		}
		row := connectors.RecordRow{Name: r.Name, Type: r.Type, Value: r.Value, Host: r.Host, Node: r.Node}
		if r.Usage != nil {
			row.Usage = connectors.UsageRow{Path: r.Usage.Path, Href: Href(*r.Usage)}
		}
		g := &groups[len(groups)-1]
		g.Rows = append(g.Rows, row)
	}
	return connectors.Records(groups), nil
}
