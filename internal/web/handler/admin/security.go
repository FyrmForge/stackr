package admin

import (
	"cmp"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	ui "github.com/FyrmForge/stackr/internal/ui/drawer/admin"
	"github.com/FyrmForge/stackr/internal/web/render"
)

const securityPage = 50

// securityActions are the actions the picker offers, with the words the
// table shows. An action not listed here shows as stored.
var securityActions = []struct{ Key, Label string }{
	{"login.ok", "sign-in"},
	{"login.bad", "sign-in failed"},
	{"login.limited", "sign-in rate limited"},
	{"key.mint", "key made"},
	{"key.revoke", "key revoked"},
	{"grant.approve", "host access approved"},
	{"grant.revoke", "host access revoked"},
	{"secrets.reveal", "secret revealed"},
	{"node.bootstrap", "controller created"},
	{"join.token", "join token issued"},
	{"join.request", "node asked to join"},
	{"node.confirm", "node joined"},
	{"node.reject", "node rejected"},
	{"cert.issue", "certificate issued"},
	{"registry.token", "registry token issued"},
	{"move.start", "env move started"},
	{"move.finish", "env move finished"},
	{"node.recover", "node recovered through the panel"},
	{"node.key_rotate", "node keys rotated"},
	{"node.cert_renew", "node certificate renewed"},
	{"node.rejoined", "node rejoined"},
	{"cert.controller_renew", "controller certificate renewed"},
	{"cert.controller_revoke", "controller certificate replaced"},
	{"ca.break_glass", "cluster root wiped"},
	{"root.rollover_start", "root rollover started"},
	{"root.rollover_push", "trust list sent to workers"},
	{"root.rollover_node", "node marked to rejoin"},
	{"root.rollover_finish", "root rollover finished"},
	{"registry.clean", "registry cleaned"},
	{"placement.create", "placement added"},
	{"placement.open", "placement sharing changed"},
	{"placement.delete", "placement deleted"},
	{"placement.node", "placement node set"},
	{"placement.move", "placement repointed"},
	{"placement.pin", "placement pinned"},
	{"placement.ingress", "ingress picked"},
	{"placement.exits", "exits picked"},
}

// actionWord is an action as the log shows it: its label, or for one not
// listed its key in plain words (no dots or underscores).
func actionWord(label map[string]string, action string) string {
	if l, ok := label[action]; ok {
		return l
	}
	return strings.NewReplacer(".", " ", "_", " ").Replace(action)
}

var securityWindows = []struct {
	Key, Label string
	Age        time.Duration
}{
	{"24h", "Last 24 hours", 24 * time.Hour},
	{"7d", "Last 7 days", 7 * 24 * time.Hour},
	{"30d", "Last 30 days", 30 * 24 * time.Hour},
}

// securityTab lists the log, newest first, filtered by ?action=, ?actor=
// (user id) and ?window=; ?limit= grows the page.
func (h *handler) securityTab(c echo.Context) (templ.Component, error) {
	ctx := c.Request().Context()
	q := ui.SecurityFilter{Action: c.QueryParam("action"), Actor: c.QueryParam("actor"), Window: c.QueryParam("window")}
	limit := securityPage
	if n, err := strconv.Atoi(c.QueryParam("limit")); err == nil && n > limit {
		limit = n
	}
	f := service.SecurityFilter{Action: q.Action, ActorID: q.Actor, Limit: limit + 1}
	for _, w := range securityWindows {
		if w.Key == q.Window {
			f.Since = time.Now().Add(-w.Age)
		}
	}
	evs, err := h.orch.SecurityLog(ctx, f)
	if err != nil {
		return nil, err
	}
	us, err := h.orch.Users(ctx)
	if err != nil {
		return nil, err
	}
	nodes, err := h.orch.NodeNames(ctx)
	if err != nil {
		return nil, err
	}
	stacks, err := h.stackNames(ctx)
	if err != nil {
		return nil, err
	}
	who := map[string]string{}
	keys := map[string]string{} // live API keys by id; a revoked one is gone
	v := ui.SecurityView{Filter: q}
	for _, u := range us {
		who[u.ID] = u.Email
		v.Actors = append(v.Actors, ui.AccessOption{Value: u.ID, Label: u.Email})
		ks, err := h.orch.Keys(ctx, u.ID) // ponytail: one query per user; fine for a server's handful
		if err != nil {
			return nil, err
		}
		for _, k := range ks {
			keys[k.ID] = k.Name
		}
	}
	label := map[string]string{}
	for _, a := range securityActions {
		label[a.Key] = a.Label
		v.Actions = append(v.Actions, ui.AccessOption{Value: a.Key, Label: a.Label})
	}
	for _, w := range securityWindows {
		v.Windows = append(v.Windows, ui.AccessOption{Value: w.Key, Label: w.Label})
	}
	if len(evs) > limit {
		evs = evs[:limit]
		more := url.Values{"limit": {strconv.Itoa(limit + securityPage)}}
		for k, s := range map[string]string{"action": q.Action, "actor": q.Actor, "window": q.Window} {
			if s != "" {
				more.Set(k, s)
			}
		}
		v.More = ui.Href("security-log") + "?" + more.Encode()
	}
	pls, err := h.orch.Placements(ctx)
	if err != nil {
		return nil, err
	}
	placement := map[string]string{}
	for _, p := range pls {
		placement[p.ID] = p.Name
	}
	paths := map[string]string{} // env, tile and org ids, worded once per page
	target := func(e service.SecurityEvent) string {
		switch e.TargetKind {
		case "node":
			if n, ok := nodes[e.TargetID]; ok {
				return "node " + n
			}
			return "a removed node"
		case "user":
			return cmp.Or(who[e.TargetID], e.TargetID)
		case "email":
			return e.TargetID
		case "api_key":
			if n, ok := keys[e.TargetID]; ok {
				return "key " + n
			}
			return "a removed key"
		case "placement":
			if p, ok := placement[e.TargetID]; ok {
				return "placement " + p
			}
			return "a removed placement"
		case "stack":
			if st, ok := stacks[e.TargetID]; ok {
				return st.org + " / " + st.name
			}
			return "a removed stack"
		case service.ScopeEnv, service.ScopeTile, service.ScopeOrg:
			k := e.TargetKind + ":" + e.TargetID
			if _, ok := paths[k]; !ok {
				paths[k] = h.orch.ScopePath(ctx, e.TargetKind, e.TargetID)
			}
			return paths[k]
		}
		if e.TargetKind == e.TargetID { // "server server"
			return e.TargetKind
		}
		return strings.TrimSpace(e.TargetKind + " " + e.TargetID)
	}
	for _, e := range evs {
		actor := "system"
		if e.ActorID != "" {
			actor = cmp.Or(who[e.ActorID], "a removed user")
		}
		r := ui.SecurityRow{
			When:    e.At,
			Actor:   actor,
			Action:  actionWord(label, e.Action),
			Target:  target(e),
			Refused: e.Action == "login.bad" || e.Action == "login.limited" || e.Action == "node.reject",
		}
		if r.Target == r.Actor { // a sign-in names its own user
			r.Target = ""
		}
		for _, d := range [][2]string{
			{"Detail", e.Detail}, {"Address", e.IP}, {"Browser", e.UserAgent}, {"Key", e.KeyID}, {"Event", e.ID},
		} {
			if d[1] != "" {
				r.Details = append(r.Details, ui.SecurityDetail{Label: d[0], Value: d[1]})
			}
		}
		v.Rows = append(v.Rows, r)
	}
	return ui.Security(v), nil
}

// masterKeyView is the master key section of Keys.
func (h *handler) masterKeyView() ui.MasterKeyView {
	st := h.orch.MasterKeyStatus()
	v := ui.MasterKeyView{Gen: st.Gen, Since: st.Since, Pending: st.Pending}
	if !st.FromEnv {
		v.Rotate = ui.Base + "/security/master-key/rotate"
	}
	return v
}

// RotateMasterKey is POST /-/admin/security/master-key/rotate. The section
// comes back with the refusal on a wrong password or an environment key, or
// with a note that the panel is restarting.
func (h *handler) RotateMasterKey(c echo.Context) error {
	v, status := h.masterKeyView(), http.StatusOK
	if err := h.orch.RotateMasterKey(c.Request().Context(), middleware.Principal(c).User.ID, c.FormValue("password")); err != nil {
		msg, ok := render.Refused(err)
		if !ok {
			return middleware.HTTPError(err)
		}
		v.Error, status = msg, http.StatusUnprocessableEntity
	} else {
		v.Done = "Rotating. The panel is restarting; reload in a moment."
	}
	return respond.HTML(c, status, ui.MasterKey(v))
}
