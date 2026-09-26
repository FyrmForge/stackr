// Package render is the one place a page meets the layout: it builds the
// shell from the request (URL scope, principal, flash, CSRF) and serves the
// full page or the #main fragment from the same handler.
package render

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/FyrmForge/hamr/pkg/htmx"
	hamrmw "github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/api/stream"
	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
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
	if drawer == nil && s.Admin && c.QueryParam("drawer") == "admin" {
		drawer = components.DrawerLoad("/-/admin?tab=" + url.QueryEscape(c.QueryParam("tab")))
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
		s.Theme = p.User.Theme
		s.Nav = append(s.Nav, link("Orgs", "/", path), link("Account", "/account", path))
		s.Admin = p.Access.Admin
	}
	sc := middleware.ScopeOf(c)
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
		href += "/" + sc.Tile.Slug
		s.Crumbs = append(s.Crumbs, link(sc.Tile.Name, href, path))
	}
	if sc.Tile == nil && href != "" && path == href { // a level's canvas: its own drawer
		s.Settings = href + "/-/drawer"
	}
	return s
}

// Theme puts the viewer's theme on the request context, so the error page
// (hamr renders it with no echo.Context) draws in it too. Mount after
// Access.Load.
func Theme(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if p := middleware.Principal(c); p != nil {
			r := c.Request()
			c.SetRequest(r.WithContext(components.WithTheme(r.Context(), p.User.Theme)))
		}
		return next(c)
	}
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

// Refused is the message of an error the user can act on (a 4xx other
// than not found), shown inline over a drawer or form; ok false = a real
// failure for the error page.
func Refused(err error) (msg string, ok bool) {
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
