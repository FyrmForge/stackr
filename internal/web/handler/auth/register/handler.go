package register

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
	"github.com/FyrmForge/stackr/internal/web/components/form"
)

// RegisterForm holds the registration form values.
type RegisterForm struct {
	Name     string `form:"name"`
	Email    string `form:"email"`
	Password string `form:"password"`
}

type handler struct {
	svc *service.Orchestrator

	FormRules validate.Form
}

// NewHandler creates a new register handler.
func NewHandler(svc *service.Orchestrator) *handler {
	return &handler{
		svc: svc,
		FormRules: validate.NewForm(
			validate.WithOOBRenderer(form.OOBValidator),
			validate.WithGeneralError("Please fix the errors below and try again."),
			validate.Field("name", validate.Required),
			validate.Field("email", validate.Required, validate.Email),
			validate.Field("password", validate.Required, validate.PasswordStrength),
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

	session, err := h.svc.Register(c.Request().Context(), f.Email, f.Password, f.Name)
	if err != nil {
		log.Warn("registration failed", "email", f.Email, "error", err)
		if v, ok := errs.IsInvalid(err); ok && v.Field == "email" {
			return respond.HTML(c, http.StatusUnprocessableEntity, registerForm(c, f, map[string]string{
				"email": "An account with this email already exists",
			}))
		}
		return respond.HTML(c, http.StatusUnprocessableEntity, registerForm(c, f, map[string]string{
			"general": "Registration failed. Please try again.",
		}))
	}

	auth.SetSession(c, h.svc.Sessions(), session)
	middleware.SetFlash(c, "Welcome! Your account has been created.", middleware.FlashSuccess)
	return respond.Redirect(c, "/")
}
