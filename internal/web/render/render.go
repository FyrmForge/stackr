// Package render is the one place a page meets the layout: it builds the
// shell from the request (URL scope, principal, flash, CSRF) and serves the
// full page or the #main fragment from the same handler.
package render

import (
	"strings"

	"github.com/FyrmForge/hamr/pkg/htmx"
	hamrmw "github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/ui/components"
)

// Page renders body inside the layout for a plain request, and as the #main
// fragment (title, header and flash out of band) for an htmx navigation. A
// history restore asks for the whole page, so it gets one.
func Page(c echo.Context, status int, title string, body templ.Component) error {
	s := Shell(c, title)
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
		s.Nav = append(s.Nav, link("Orgs", "/", path), link("Account", "/account", path))
		if p.Access.Admin {
			s.Nav = append(s.Nav, link("Admin", "/admin", path))
		}
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
		href += "/" + sc.Env.Slug
		s.Crumbs = append(s.Crumbs, link(sc.Env.Name, href, path))
	}
	if sc.Tile != nil {
		href += "/" + sc.Tile.Slug
		s.Crumbs = append(s.Crumbs, link(sc.Tile.Name, href, path))
	}
	return s
}

// link is active when the URL is its page or below it; "/" only on itself.
func link(label, href, path string) components.Link {
	active := path == href || (href != "/" && strings.HasPrefix(path, href+"/"))
	return components.Link{Label: label, Href: href, Active: active}
}
