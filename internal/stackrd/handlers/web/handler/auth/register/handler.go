package register

import (
	"errors"
	"net/http"
	"strings"

	hamrauth "github.com/FyrmForge/hamr/pkg/auth"
	"github.com/FyrmForge/hamr/pkg/logging"
	"github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/FyrmForge/hamr/pkg/validate"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/components/form"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/handler/auth"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
)

// RegisterForm holds the registration form values.
type RegisterForm struct {
	Name     string `form:"name"`
	Email    string `form:"email"`
	Password string `form:"password"`
}

type handler struct {
	authService    *service.AuthService
	sessionManager *hamrauth.SessionManager

	FormRules validate.Form
}

// NewHandler creates a new register handler.
func NewHandler(authService *service.AuthService, sm *hamrauth.SessionManager) *handler {
	return &handler{
		authService:    authService,
		sessionManager: sm,
		FormRules: validate.NewForm(
			validate.WithOOBRenderer(form.OOBValidator),
			validate.WithGeneralError("Please fix the errors below and try again."),
			validate.Field("name", validate.Required),
			validate.Field("email", validate.Required, validate.Email),
			validate.Field("password", validate.Required, validate.PasswordStrength),
			validate.Field("confirm_password", validate.Required).
				WithCtx(func(c echo.Context, value string) string {
					if value != c.FormValue("password") {
						return "Passwords do not match"
					}
					return ""
				}),
		),
	}
}

// GET /register
func (h *handler) Page(c echo.Context) error {
	return respond.HTML(c, http.StatusOK, registerPage(c, RegisterForm{}, nil))
}

// POST /register
func (h *handler) Submit(c echo.Context) error {
	var f RegisterForm
	if err := c.Bind(&f); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid form data")
	}
	f.Email = strings.ToLower(f.Email)

	if errs := h.FormRules.Validate(c); errs != nil {
		return respond.HTML(c, http.StatusUnprocessableEntity, registerForm(c, f, errs))
	}

	log := logging.FromContext(c.Request().Context())

	user, err := h.authService.Register(c.Request().Context(), f.Email, f.Password, f.Name)
	if err != nil {
		log.Warn("registration failed", "email", f.Email, "error", err)
		if errors.Is(err, service.ErrEmailTaken) {
			return respond.HTML(c, http.StatusUnprocessableEntity, registerForm(c, f, map[string]string{
				"email": "An account with this email already exists",
			}))
		}
		return respond.HTML(c, http.StatusUnprocessableEntity, registerForm(c, f, map[string]string{
			"general": "Registration failed. Please try again.",
		}))
	}

	session, err := h.sessionManager.CreateSession(c.Request().Context(), user.ID, nil)
	if err != nil {
		log.Error("create session failed", "error", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "session error")
	}

	auth.SetSession(c, h.sessionManager, session)
	middleware.SetFlash(c, "Welcome! Your account has been created.", middleware.FlashSuccess)
	// Registration only happens on first boot, when there are no orgs yet, so
	// the next thing anyone needs is the onboarding wizard rather than an empty
	// root canvas.
	return respond.Redirect(c, "/setup")
}
