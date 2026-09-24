package web

import (
	"github.com/FyrmForge/hamr/pkg/email"
	hamrmw "github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/server"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/ui/components"
	"github.com/FyrmForge/stackr/internal/web/handler/about"
	"github.com/FyrmForge/stackr/internal/web/handler/auth/login"
	"github.com/FyrmForge/stackr/internal/web/handler/auth/register"
	"github.com/FyrmForge/stackr/internal/web/handler/canvas"
	"github.com/FyrmForge/stackr/internal/web/handler/devemail"
	"github.com/FyrmForge/stackr/internal/web/handler/devgallery"
	"github.com/FyrmForge/stackr/internal/web/handler/env"
	"github.com/FyrmForge/stackr/internal/web/handler/scope"
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
		if l.path == "" { // home sends a visitor to the login page
			site.GET("/", cv.Page, auth.RequireAuth(), l.read)
		} else { // deeper pages answer 401/404 like the API (the access test)
			site.GET(l.path, cv.Page, l.read)
		}
		if l.path != "/:org/:stack/:env" { // the env stream is the env package's
			site.GET(l.path+"/-/events", cv.Events, l.read)
		}
		site.POST(l.path+"/-/positions", cv.Positions, l.write)
		site.POST(l.path+"/-/reset", cv.Reset, l.write)
		site.POST(l.path+"/-/notes", cv.Notes, l.write)
		site.POST(l.path+"/-/notes/delete", cv.DeleteNote, l.write)
	}

	cv.Mount(site, deps.Access)

	// ponytail: the tile page is a placeholder until the tile drawer takes it.
	scopeHandler := scope.NewHandler()
	site.GET("/:org/:stack/:env/:tile", scopeHandler.Page, deps.Access.Require("tile.read"))

	// The env canvas's stream, drawers and dialogs (tasks 9 and 10). A
	// static segment wins over :tile in echo, so these words shadow a tile
	// of the same slug (DECIDE).
	envH := env.NewHandler(deps.Service)
	site.GET("/:org/:stack/:env/events", envH.Events, deps.Access.Require("tile.read"))
}

// RegisterStaticPages registers handlers for static generation and runtime
// serving. Each call to StaticPage registers both a generation entry and a
// GET route. These handlers must not depend on database or session state.
func RegisterStaticPages(srv *server.Server) {
	h := about.NewHandler()
	srv.StaticPage("/about", h.About)
}
