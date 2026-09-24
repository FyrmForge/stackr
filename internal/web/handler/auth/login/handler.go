package login

import (
	"net/http"
	"strings"

	"github.com/FyrmForge/hamr/pkg/logging"
	"github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/FyrmForge/hamr/pkg/validate"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/auth"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/ui/components"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// LoginForm holds the login form values.
type LoginForm struct {
	Email    string `form:"email"`
	Password string `form:"password"`
}

// handler owns the login page plus its sibling logout action. Logout lives
// here because it's the inverse of login, not a page of its own.
type handler struct {
	svc *service.Orchestrator

	FormRules validate.Form
}

// NewHandler creates a new login handler.
func NewHandler(svc *service.Orchestrator) *handler {
	return &handler{
		svc: svc,
		FormRules: validate.NewForm(
			validate.WithOOBRenderer(components.OOBValidator),
			validate.Field("email", validate.Required, validate.Email),
			validate.Field("password", validate.Required),
		),
	}
}

// GET /login
func (h *handler) Page(c echo.Context) error {
	return render.Page(c, http.StatusOK, "Log In", loginPage(c, LoginForm{}, nil))
}

// POST /login
func (h *handler) Submit(c echo.Context) error {
	var f LoginForm
	if err := c.Bind(&f); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid form data")
	}
	f.Email = strings.ToLower(f.Email)

	if errs := h.FormRules.Validate(c); errs != nil {
		return respond.HTML(c, http.StatusUnprocessableEntity, loginForm(c, f, errs))
	}

	log := logging.FromContext(c.Request().Context())

	session, err := h.svc.Login(c.Request().Context(), f.Email, f.Password)
	if _, bad := errs.IsInvalid(err); bad {
		log.Warn("login failed", "email", f.Email)
		return respond.HTML(c, http.StatusUnauthorized, loginForm(c, f, map[string]string{
			"general": "Invalid email or password",
		}))
	}
	if err != nil {
		log.Error("login failed", "error", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "session error")
	}

	auth.SetSession(c, h.svc.Sessions(), session)
	return respond.Redirect(c, "/")
}

// POST /logout
func (h *handler) Logout(c echo.Context) error {
	sm := h.svc.Sessions()
	if cookie, err := c.Cookie(sm.CookieName()); err == nil {
		_ = h.svc.Logout(c.Request().Context(), cookie.Value)
	}

	auth.ClearSession(c, sm)
	middleware.SetFlash(c, "You have been logged out", middleware.FlashInfo)
	return respond.Redirect(c, "/login")
}
