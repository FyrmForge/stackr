// Package render is the one place a page meets the layout: it builds the
// shell from the request (URL scope, principal, flash, CSRF) and serves the
// full page or the #main fragment from the same handler.
package render

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/FyrmForge/hamr/pkg/htmx"
	hamrmw "github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/api/stream"
	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/ui/cluster"
	"github.com/FyrmForge/stackr/internal/ui/components"
)

// Page renders body inside the layout for a plain request, and as the #main
// fragment (title, rail, top bar and flash out of band) for an htmx
// navigation. A history restore asks for the whole page, so it gets one.
func Page(c echo.Context, status int, title string, body templ.Component) error {
	return PageWith(c, status, title, body, nil, nil)
}

// PageWith is Page with the top bar's actions (nil = none; they show only
// where the bar does) and the drawer a fresh load of ?drawer=&tab= opens
// (nil = none). An htmx navigation leaves the drawer to its own GET.
func PageWith(c echo.Context, status int, title string, body, actions, drawer templ.Component) error {
	s := Shell(c, title)
	s.Actions = actions
	// a node's drawer opens over any page
	if d := c.QueryParam("drawer"); drawer == nil && s.Admin {
		if id, ok := strings.CutPrefix(d, "node:"); ok && id != "" {
			drawer = components.DrawerLoad(cluster.NodeDrawer(id, c.QueryParam("tab")))
		}
	}
	s.Drawer, s.Tab = drawer, c.QueryParam("tab")
	if drawer == nil {
		s.Tab = ""
	}
	c.Response().Header().Add("Vary", htmx.HeaderRequest)
	r := c.Request()
	if htmx.IsHTMX(r) && r.Header.Get(htmx.HeaderHistoryRestore) != "true" {
		return respond.HTML(c, status, components.Fragment(s, body))
	}
	return respond.HTML(c, status, components.Layout(s, body))
}

// Bare is a page with no rail and no top bar, the setup wizard's (v0's
// chromeless Layout), full page even for htmx: its links navigate.
func Bare(c echo.Context, status int, title string, body templ.Component) error {
	s := Shell(c, title)
	s.User, s.Nav, s.Crumbs, s.Envs, s.Settings, s.Problems = "", nil, nil, nil, "", nil
	return respond.HTML(c, status, components.Layout(s, body))
}

// Shell reads what the layout shows off the request. Every read tolerates
// its middleware being absent (static generation, tests).
func Shell(c echo.Context, title string) components.Shell {
	s := components.Shell{Title: title}
	s.CSRF, _ = c.Get("csrf").(string)
	if f := hamrmw.GetFlash(c); f != nil {
		s.Flash = components.Flash{Message: f.Message, Kind: string(f.Type)}
	}
	path := c.Request().URL.Path
	if p := middleware.Principal(c); p != nil {
		s.User = p.User.Email
		s.Theme, s.TimeZone = p.User.Theme, p.User.TimeZone
		s.Nav = append(s.Nav, link("Orgs", "/", path), link("Account", "/account", path))
		s.Admin = p.Access.Admin
		s.AdminOn = s.Admin && strings.HasPrefix(path, "/-/admin/")
		if s.Admin { // the Cluster map is the admin's
			s.Nav = append(s.Nav, link("Cluster", "/-/cluster", path))
		}
		if count, ok := c.Get(waitingKey).(func() int); ok && s.Admin {
			s.Waiting = count()
		}
		if ps, ok := c.Get(problemsKey).(func() []service.Problem); ok && s.Admin {
			for _, p := range ps() {
				s.Problems = append(s.Problems, components.Problem{Critical: p.Severity == service.SevCritical,
					Message: p.Message, Since: p.Since, Href: p.Link, LinkText: p.LinkText})
			}
			s.ProblemsMore = service.ProblemsHref
		}
	}
	sc := middleware.ScopeOf(c)
	if sc.Env != nil {
		if down, ok := c.Get(nodeDownKey).(func(envID string) string); ok {
			s.NodeDown = down(sc.Env.ID)
		}
		if s.NodeDown == "" && sc.Org != nil && sc.Stack != nil {
			if d, ok := c.Get(ingressDownKey).(func(envID string) string); ok {
				if s.NodeDown = d(sc.Env.ID); s.NodeDown != "" {
					s.MoveIngress = "/" + sc.Org.Slug + "/" + sc.Stack.Slug + "/" + sc.Env.Slug + "/-/drawer?tab=settings&open=placement"
				}
			}
		}
	}
	href := ""
	if sc.Org != nil {
		href += "/" + sc.Org.Slug
		s.Crumbs = append(s.Crumbs, link(sc.Org.Name, href, path))
	}
	if sc.Stack != nil {
		href += "/" + sc.Stack.Slug
		s.Crumbs = append(s.Crumbs, link(sc.Stack.Name, href, path))
	}
	if sc.Env != nil {
		hues := service.EnvHues(sc.Envs)
		for _, e := range sc.Envs {
			l := link(e.Name, href+"/"+e.Slug, path)
			l.Color = hues[e.ID]
			s.Envs = append(s.Envs, l)
		}
		href += "/" + sc.Env.Slug
		s.Crumbs = append(s.Crumbs, link(sc.Env.Name, href, path))
		s.EnvColor = hues[sc.Env.ID]
		if s.EnvColor == "" { // a scope built without its env list
			s.EnvColor = sc.Env.Color
		}
	}
	if sc.Tile != nil {
		s.Settings = href + "/-/tiles/" + sc.Tile.Slug + "?tab=settings"
		href += "/" + sc.Tile.Slug
		s.Crumbs = append(s.Crumbs, link(sc.Tile.Name, href, path))
	}
	if sc.Tile == nil && href != "" && path == href { // a level's canvas: its own drawer
		s.Settings = href + "/-/drawer?tab=settings"
	}
	return s
}

const waitingKey = "host_waiting"
const problemsKey = "admin_problems"
const nodeDownKey = "env_node_down"
const ingressDownKey = "env_ingress_down"

// NodeDown lets the shell show the offline banner on env and tile pages. It
// leaves a lazy func, which Shell calls only when the URL has an env; the
// answer is "" while the env's node is up.
func NodeDown(orch *service.Orchestrator) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Set(nodeDownKey, func(envID string) string {
				d, ok := orch.EnvNodeDown(c.Request().Context(), envID)
				if !ok {
					return ""
				}
				const tail = ". Deploys here are refused until it is back."
				down := &service.NodeDownError{Name: d.Name, LastSeen: d.LastSeen}
				if p := middleware.Principal(c); p != nil && authz.Can(p.Access, "node.manage", authz.Resource{}) == nil {
					return down.Detail() + tail
				}
				if d.Placement == "" {
					return middleware.NodeOfflineMsg + " Deploys here are refused until it is back."
				}
				// members get the placement, never the node (D3)
				return "Placement " + d.Placement + " is offline, " + down.Since() + tail
			})
			c.Set(ingressDownKey, func(envID string) string {
				d, ok := orch.EnvIngressDown(c.Request().Context(), envID)
				if !ok {
					return ""
				}
				hosts := strings.Join(d.Hosts, ", ")
				if p := middleware.Principal(c); p != nil && authz.Can(p.Access, "node.manage", authz.Resource{}) == nil {
					return "Ingress node " + d.Node + " is down: " + hosts + " stop answering."
				}
				return "The ingress serving " + hosts + " is offline."
			})
			return next(c)
		}
	}
}

// Waiting lets the shell show the admin rail's badge and the problem banner:
// for a server admin it leaves a lazy count of stacks waiting on elevated
// access and a lazy list of open problems, which Shell
// reads only when a page draws the rail (assets and API calls never pay).
// Mount after Access.Load.
func Waiting(orch *service.Orchestrator) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if p := middleware.Principal(c); p != nil && p.Access.Admin {
				c.Set(waitingKey, func() int {
					n, _ := orch.HostGrantsWaiting(c.Request().Context())
					return n
				})
				c.Set(problemsKey, func() []service.Problem {
					ps, _ := orch.Problems(c.Request().Context())
					return ps
				})
			}
			return next(c)
		}
	}
}

// Theme puts the viewer's theme, the request id and the CSRF token on the
// request context, so the error page (hamr renders it with no echo.Context)
// draws in the theme, shows the id and can log out. Mount after Access.Load,
// the CSRF and the logging middleware.
func Theme(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		csrf, _ := c.Get("csrf").(string)
		r := c.Request()
		c.SetRequest(r.WithContext(components.WithRequest(r.Context(), c.Response().Header().Get(middleware.HeaderRequestID), csrf)))
		if p := middleware.Principal(c); p != nil {
			r = c.Request()
			c.SetRequest(r.WithContext(components.WithTheme(r.Context(), p.User.Theme)))
		}
		return next(c)
	}
}

// DrawerTab pushes ?drawer=&tab= for a drawer tab picked from the phone
// select, as the tab links do: the select sends its node as ?drawer=, the
// links do not. A reply that is not a 200 pushes nothing.
func DrawerTab(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		r := c.Request()
		node, tab := c.QueryParam("drawer"), c.QueryParam("tab")
		if node != "" && tab != "" && r.Header.Get(htmx.HeaderTarget) == components.DrawerRoot {
			res := c.Response()
			res.Before(func() {
				if res.Status == http.StatusOK {
					res.Header().Set(htmx.HeaderPushURL, "?drawer="+url.QueryEscape(node)+"&tab="+url.QueryEscape(tab))
				}
			})
		}
		return next(c)
	}
}

// ToShell answers a plain load of a drawer route (its link opened in a new
// tab), which would be a bare fragment, with a redirect to the page under
// "/-/" that opens drawer node there: ?drawer=&tab=. ok false = an htmx
// request, for the route itself.
func ToShell(c echo.Context, node string) (ok bool, err error) {
	return ToShellTab(c, node, c.QueryParam("tab"))
}

// ToShellTab is ToShell opening tab, for a route that is one tab of a
// drawer (placement, log history). node "" is the page alone: a dialog
// with no link of its own.
func ToShellTab(c echo.Context, node, tab string) (ok bool, err error) {
	if htmx.IsHTMX(c.Request()) {
		return false, nil
	}
	page, _, _ := strings.Cut(c.Request().URL.Path, "/-/")
	q := url.Values{}
	if node != "" {
		q.Set("drawer", node)
		if tab != "" {
			q.Set("tab", tab)
		}
	}
	to := cmp.Or(page, "/")
	if len(q) > 0 {
		to += "?" + q.Encode()
	}
	return true, c.Redirect(http.StatusSeeOther, to)
}

// link is active when the URL is its page or below it; "/" only on itself.
func link(label, href, path string) components.Link {
	active := path == href || (href != "/" && strings.HasPrefix(path, href+"/"))
	return components.Link{Label: label, Href: href, Active: active}
}

// Event renders a component as an SSE event body for htmx to swap.
func Event(ctx context.Context, body templ.Component) (stream.HTML, error) {
	var b strings.Builder
	err := body.Render(ctx, &b)
	return stream.HTML(b.String()), err
}

// Refusal is Refused with an invalid field's own sentence, no field name
// in front: for a form that already marks the field, or a dialog.
func Refusal(err error) (msg string, ok bool) {
	if inv, ok := errs.IsInvalid(err); ok {
		return inv.Msg, true
	}
	return Refused(err)
}

// Refused is the message of an error the user can act on (a 4xx other
// than not found), shown inline over a drawer or form; ok false = a real
// failure for the error page.
func Refused(err error) (msg string, ok bool) {
	var down *service.NodeDownError // a 503, but one the user acts on: start the node
	if errors.As(err, &down) {
		// No name: env readers must not learn which node; an admin has it
		// from the offline banner.
		return middleware.NodeOfflineMsg, true
	}
	var he *echo.HTTPError
	if errors.As(middleware.HTTPError(err), &he) && he.Code < 500 && he.Code != 404 {
		return fmt.Sprint(he.Message), true
	}
	return "", false
}

// EnvURL is the env page's path from the request's scope: /org/stack/env.
func EnvURL(c echo.Context) string {
	s := middleware.ScopeOf(c)
	return "/" + s.Org.Slug + "/" + s.Stack.Slug + "/" + s.Env.Slug
}

// JobView is a job's status line; page is the path its job stream hangs
// under (an env page, or "" for the admin drawer's panel jobs).
func JobView(page string, j service.Job) components.JobStatusView {
	return components.JobStatusView{
		Kind:      j.Kind,
		State:     j.State,
		Error:     j.Error,
		StreamURL: page + "/-/jobs/" + j.ID + "/events",
		Live:      j.FinishedAt == nil,
	}
}

// OrgPlanView is the org file's plan as the plan component draws it: its
// notes read as warnings do, and a new domain or param is a create (the
// component's +) naming its noun. The org drawer's Config tab shows it.
func OrgPlanView(p service.OrgConfigPlan) components.PlanView {
	pv := components.PlanView{
		Title:     "What changes",
		Blockers:  p.Blockers,
		Warnings:  p.Notes,
		CanDeploy: !p.Blocked(),
	}
	for _, ch := range p.Changes {
		cv := components.ChangeView{
			Kind:     ch.Kind,
			Tile:     ch.Tile,
			Field:    ch.Field,
			Old:      ch.Old,
			New:      ch.New,
			Note:     ch.Note,
			Impact:   ch.Impact,
			Key:      ch.Key,
			Optional: ch.Optional,
		}
		switch ch.Kind {
		case "domain", "param", "share":
			if !ch.Optional {
				cv.Kind = "create"
				cv.Tile = strings.TrimSpace(ch.Kind + " " + ch.Tile)
			}
		}
		pv.Changes = append(pv.Changes, cv)
	}
	return pv
}

// JobStream answers GET <page>/-/jobs/:job/events: the job's status body as
// "update" whenever it changes, the final one included, then "end".
func JobStream(c echo.Context, orch *service.Orchestrator, page string) error {
	id := c.Param("job")
	last, seq, ended := stream.HTML(""), int64(0), false
	return stream.PollAs(c, "update", func(ctx context.Context, _ int64) (any, int64, bool, error) {
		if ended {
			return last, seq, true, nil
		}
		j, err := orch.GetJob(ctx, id)
		if err != nil {
			return nil, 0, false, err
		}
		b, err := Event(ctx, components.JobStatusBody(JobView(page, j)))
		if err != nil {
			return nil, 0, false, err
		}
		if b != last {
			last, seq = b, seq+1
		}
		ended = j.FinishedAt != nil
		return last, seq, false, nil
	})
}

// Size is a byte count for people.
func Size(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
	}
	return fmt.Sprintf("%d KB", b>>10)
}
