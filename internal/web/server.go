package web

import (
	"context"

	hamrmw "github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/server"
	"github.com/FyrmForge/hamr/pkg/auth"
	"github.com/FyrmForge/hamr/pkg/storage"
	"github.com/FyrmForge/hamr/pkg/email"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/repo"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/web/handler/auth/login"
	"github.com/FyrmForge/stackr/internal/web/handler/auth/register"
	"github.com/FyrmForge/stackr/internal/web/handler/about"
	"github.com/FyrmForge/stackr/internal/web/handler/devemail"
	"github.com/FyrmForge/stackr/internal/web/handler/home"
	"github.com/FyrmForge/stackr/internal/web/components"
)

// Deps holds the dependencies for route registration.
type Deps struct {
	Store          repo.Store
	BaseURL        string
	StaticBaseURL  string
	DevMode        bool
	SessionManager *auth.SessionManager
	AuthService *service.AuthService
	FileStorage storage.FileStorage
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

	auth := hamrmw.NewBrowserAuth(deps.SessionManager,
		hamrmw.WithSubjectLoader(func(reqCtx context.Context, id string) (any, error) {
			return deps.Store.GetUserByID(reqCtx, id)
		}),
		hamrmw.WithLoginRedirect("/login"),
		hamrmw.WithHomeRedirect("/"),
	)
	site.Use(auth.Load())

	homeHandler := home.NewHandler()
	site.GET("/", homeHandler.Index)

	// Dev-only sample email endpoint. Sends a test message through the
	// configured email.Sender so you can smoke-test the /__hamr/mail inbox.
	// Gated on DevMode; no-op in production regardless of route registration.
	if deps.EmailSender != nil {
		devmailHandler := devemail.NewHandler(deps.EmailSender, deps.DevMode)
		site.GET("/dev/send-test-email", devmailHandler.SendTest)
	}

	// Auth routes — one page-package per page (login owns logout as its inverse action).
	loginHandler := login.NewHandler(deps.AuthService, deps.SessionManager)
	site.GET("/login", loginHandler.Page, auth.RequireNotAuth())
	site.POST("/login", loginHandler.Submit, auth.RequireNotAuth())
	site.POST("/login/validate/:field", loginHandler.FormRules.ValidationHandler("field"), auth.RequireNotAuth())
	site.POST("/logout", loginHandler.Logout, auth.RequireAuth())

	registerHandler := register.NewHandler(deps.AuthService, deps.SessionManager)
	site.GET("/register", registerHandler.Page, auth.RequireNotAuth())
	site.POST("/register", registerHandler.Submit, auth.RequireNotAuth())
	site.POST("/register/validate/:field", registerHandler.FormRules.ValidationHandler("field"), auth.RequireNotAuth())
}

// RegisterStaticPages registers handlers for static generation and runtime
// serving. Each call to StaticPage registers both a generation entry and a
// GET route. These handlers must not depend on database or session state.
func RegisterStaticPages(srv *server.Server) {
	h := about.NewHandler()
	srv.StaticPage("/about", h.About)
}
