package register

import (
	"cmp"
	"net/http"

	"github.com/FyrmForge/hamr/pkg/logging"
	hamrmw "github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/FyrmForge/hamr/pkg/validate"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/auth"
	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/ui/components"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// RegisterForm holds the registration form values.
type RegisterForm struct {
	Name     string `form:"name"`
	Email    string `form:"email"`
	Password string `form:"password"`
	Next     string `form:"next"`
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
			validate.WithOOBRenderer(components.OOBValidator),
			validate.WithGeneralError("Please fix the errors below and try again."),
			validate.Field("name", validate.Required),
			validate.Field("email", validate.Required, validate.Email),
			validate.Field("password", validate.Required, validate.PasswordStrength),
		),
	}
}

// GET /register
func (h *handler) Page(c echo.Context) error {
	return render.Page(c, http.StatusOK, "Register", registerPage(RegisterForm{Next: middleware.SafeNext(c.QueryParam("next"))}, nil))
}

// POST /register
func (h *handler) Submit(c echo.Context) error {
	var f RegisterForm
	if err := c.Bind(&f); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid form data")
	}
	f.Next = middleware.SafeNext(f.Next)

	if errs := h.FormRules.Validate(c); errs != nil {
		return respond.HTML(c, http.StatusUnprocessableEntity, registerForm(f, errs))
	}

	log := logging.FromContext(c.Request().Context())

	session, err := h.svc.Register(c.Request().Context(), f.Email, f.Password, f.Name)
	if err != nil {
		log.Warn("registration failed", "email", f.Email, "error", err)
		if v, ok := errs.IsInvalid(err); ok && v.Field == "email" {
			return respond.HTML(c, http.StatusUnprocessableEntity, registerForm(f, map[string]string{
				"email": "An account with this email already exists",
			}))
		}
		return respond.HTML(c, http.StatusUnprocessableEntity, registerForm(f, map[string]string{
			"general": "Registration failed. Please try again.",
		}))
	}

	auth.SetSession(c, h.svc.Sessions(), session)
	hamrmw.SetFlash(c, "Welcome! Your account has been created.", hamrmw.FlashSuccess)
	return respond.Redirect(c, cmp.Or(f.Next, "/setup"))
}
