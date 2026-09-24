package login

import (
	"net/http"
	"strings"

	hamrauth "github.com/FyrmForge/hamr/pkg/auth"
	"github.com/FyrmForge/hamr/pkg/logging"
	"github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/FyrmForge/hamr/pkg/validate"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/auth"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/web/components/form"
)

// LoginForm holds the login form values.
type LoginForm struct {
	Email    string `form:"email"`
	Password string `form:"password"`
}

// handler owns the login page plus its sibling logout action. Logout lives
// here because it's the inverse of login, not a page of its own.
type handler struct {
	authService    *service.AuthService
	sessionManager *hamrauth.SessionManager

	FormRules validate.Form
}

// NewHandler creates a new login handler.
func NewHandler(authService *service.AuthService, sm *hamrauth.SessionManager) *handler {
	return &handler{
		authService:    authService,
		sessionManager: sm,
		FormRules: validate.NewForm(
			validate.WithOOBRenderer(form.OOBValidator),
			validate.Field("email", validate.Required, validate.Email),
			validate.Field("password", validate.Required),
		),
	}
}

// GET /login
func (h *handler) Page(c echo.Context) error {
	return respond.HTML(c, http.StatusOK, loginPage(c, LoginForm{}, nil))
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

	user, err := h.authService.Authenticate(c.Request().Context(), f.Email, f.Password)
	if err != nil {
		log.Warn("login failed", "email", f.Email, "error", err)
		return respond.HTML(c, http.StatusUnauthorized, loginForm(c, f, map[string]string{
			"general": "Invalid email or password",
		}))
	}

	session, err := h.sessionManager.CreateSession(c.Request().Context(), user.ID, nil)
	if err != nil {
		log.Error("create session failed", "error", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "session error")
	}

	auth.SetSession(c, h.sessionManager, session)
	return respond.Redirect(c, "/")
}

// POST /logout
func (h *handler) Logout(c echo.Context) error {
	cookie, err := c.Cookie(h.sessionManager.CookieName())
	if err == nil {
		session, _ := h.sessionManager.ValidateSession(c.Request().Context(), cookie.Value)
		if session != nil {
			_ = h.sessionManager.DeleteSession(c.Request().Context(), session.ID)
		}
	}

	auth.ClearSession(c, h.sessionManager)
	middleware.SetFlash(c, "You have been logged out", middleware.FlashInfo)
	return respond.Redirect(c, "/login")
}
