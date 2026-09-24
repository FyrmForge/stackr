package web

import (
	"cmp"

	"github.com/FyrmForge/hamr/pkg/email"
	hamrmw "github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/server"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/ui/components"
	"github.com/FyrmForge/stackr/internal/web/handler/about"
	"github.com/FyrmForge/stackr/internal/web/handler/account"
	"github.com/FyrmForge/stackr/internal/web/handler/auth/cliauth"
	"github.com/FyrmForge/stackr/internal/web/handler/auth/invite"
	"github.com/FyrmForge/stackr/internal/web/handler/auth/login"
	"github.com/FyrmForge/stackr/internal/web/handler/auth/register"
	"github.com/FyrmForge/stackr/internal/web/handler/auth/setup"
	"github.com/FyrmForge/stackr/internal/web/handler/canvas"
	"github.com/FyrmForge/stackr/internal/web/handler/devemail"
	"github.com/FyrmForge/stackr/internal/web/handler/devgallery"
	"github.com/FyrmForge/stackr/internal/web/handler/env"
	"github.com/FyrmForge/stackr/internal/web/handler/scope"
	"github.com/FyrmForge/stackr/internal/web/handler/tile"
)

// Deps holds the dependencies for route registration.
type Deps struct {
	Service       *service.Orchestrator
	Access        *middleware.Access // shared with the API router
	BaseURL       string
	StaticBaseURL string
	DevMode       bool
	// EmailSender is the outbound mail transport. In dev it points at the
	// hamr email inbox at /__hamr/mail (via pkg/emailmock); in production
	// swap in a real provider adapter that satisfies email.Sender.
	EmailSender email.Sender
}

// RegisterRoutes registers all web route handlers on the server.
func RegisterRoutes(srv *server.Server, deps *Deps) {
	e := srv.Echo()

	// Content Security Policy.
	csp := "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'"

	// Site routes.
	site := e.Group("")
	site.Use(middleware.Logging())
	site.Use(hamrmw.ErrorPages(components.ErrorPage))
	site.Use(hamrmw.SecureWithConfig(hamrmw.SecureConfig{
		ContentSecurityPolicy: csp,
	}))
	site.Use(hamrmw.FlashWithConfig(hamrmw.FlashConfig{Secure: !deps.DevMode}))
	site.Use(hamrmw.CSRFWithConfig(hamrmw.CSRFConfig{Secure: !deps.DevMode}))

	site.Use(deps.Access.Load())
	auth := deps.Access.Browser()

	// Dev-only sample email endpoint. Sends a test message through the
	// configured email.Sender so you can smoke-test the /__hamr/mail inbox.
	// Gated on DevMode; no-op in production regardless of route registration.
	if deps.EmailSender != nil {
		devmailHandler := devemail.NewHandler(deps.EmailSender, deps.DevMode)
		site.GET("/dev/send-test-email", devmailHandler.SendTest)
	}

	// Every shared component with sample views (dev only).
	if deps.DevMode {
		gallery := devgallery.NewHandler()
		site.GET("/dev/components", gallery.Page)
		site.POST("/dev/components/positions", gallery.Positions)
		site.GET("/dev/components/drawer", gallery.Drawer)
	}

	// Auth routes — one page-package per page (login owns logout as its inverse action).
	loginHandler := login.NewHandler(deps.Service)
	site.GET("/login", loginHandler.Page, auth.RequireNotAuth())
	site.POST("/login", loginHandler.Submit, auth.RequireNotAuth())
	site.POST("/login/validate/:field", loginHandler.FormRules.ValidationHandler("field"), auth.RequireNotAuth())
	site.POST("/logout", loginHandler.Logout, auth.RequireAuth())

	registerHandler := register.NewHandler(deps.Service)
	site.GET("/register", registerHandler.Page, auth.RequireNotAuth())
	site.POST("/register", registerHandler.Submit, auth.RequireNotAuth())
	site.POST("/register/validate/:field", registerHandler.FormRules.ValidationHandler("field"), auth.RequireNotAuth())

	// A page a visitor may not see sends them to log in and back.
	page, authed := deps.Access.LoginFirst(), deps.Access.Authed()

	site.GET("/setup", setup.NewHandler(deps.Service).Page, page, authed)

	inv := invite.NewHandler(deps.Service)
	site.GET("/invite/:token", inv.Page)
	site.POST("/invite/:token", inv.Accept, authed)
	site.POST("/invite/:token/register", inv.Register, auth.RequireNotAuth())

	cli := cliauth.NewHandler(deps.Service)
	site.GET("/cli/authorize", cli.Page, page, authed)
	site.POST("/cli/authorize/:org", cli.Approve, deps.Access.Require("org.read"))

	acct := account.NewHandler(deps.Service)
	site.GET("/account", acct.Page, page, authed)
	site.POST("/account/password", acct.Password, authed)
	site.POST("/account/keys/:key/revoke", acct.Revoke, authed)

	// The canvases, one per level. Require resolves each slug, 404s early
	// and leaves org, stack and env in the context; home is the caller's.
	// Each level's helper routes sit under "/-/", which no slug can be.
	cv := canvas.NewHandler(deps.Service)
	levels := []struct {
		path        string
		read, write echo.MiddlewareFunc
	}{
		{"", deps.Access.Authed(), deps.Access.Authed()},
		{"/:org", deps.Access.Require("org.read"), deps.Access.Require("org.graph.write")},
		{"/:org/:stack", deps.Access.Require("org.read"), deps.Access.Require("stack.write")},
		{"/:org/:stack/:env", deps.Access.Require("org.read"), deps.Access.Require("env.write")},
	}
	for _, l := range levels {
		site.GET(cmp.Or(l.path, "/"), cv.Page, page, l.read)
		site.GET(l.path+"/-/events", cv.Events, l.read)
		site.POST(l.path+"/-/positions", cv.Positions, l.write)
		site.POST(l.path+"/-/reset", cv.Reset, l.write)
		site.POST(l.path+"/-/notes", cv.Notes, l.write)
		site.POST(l.path+"/-/notes/delete", cv.DeleteNote, l.write)
	}

	cv.Mount(site, deps.Access)

	// ponytail: the tile page is a placeholder until the tile drawer takes it.
	scopeHandler := scope.NewHandler()
	site.GET("/:org/:stack/:env/:tile", scopeHandler.Page, page, deps.Access.Require("tile.read"))

	// The env canvas's drawers and dialogs (tasks 9 and 10), under /-/ too.
	env.NewHandler(deps.Service).Mount(site, deps.Access)
	tile.NewHandler(deps.Service).Mount(site, deps.Access)
}

// RegisterStaticPages registers handlers for static generation and runtime
// serving. Each call to StaticPage registers both a generation entry and a
// GET route. These handlers must not depend on database or session state.
func RegisterStaticPages(srv *server.Server) {
	h := about.NewHandler()
	srv.StaticPage("/about", h.About)
}
