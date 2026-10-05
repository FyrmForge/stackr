package login

import (
	"cmp"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

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

// LoginForm holds the login form values.
type LoginForm struct {
	Email    string `form:"email"`
	Password string `form:"password"`
	Next     string `form:"next"`
}

// handler owns the login page plus its sibling logout action. Logout lives
// here because it's the inverse of login, not a page of its own.
type handler struct {
	orch *service.Orchestrator

	FormRules validate.Form
}

// NewHandler creates a new login handler.
func NewHandler(orch *service.Orchestrator) *handler {
	return &handler{
		orch: orch,
		FormRules: validate.NewForm(
			validate.WithOOBRenderer(components.OOBValidator),
			validate.Field("email", validate.Required, validate.Email),
			validate.Field("password", validate.Required),
		),
	}
}

// GET /login
func (h *handler) Page(c echo.Context) error {
	return render.Page(
		c,
		http.StatusOK,
		"Log in",
		loginPage(LoginForm{Next: middleware.SafeNext(c.QueryParam("next"))}, nil),
	)
}

// POST /login
func (h *handler) Submit(c echo.Context) error {
	var f LoginForm
	if err := c.Bind(&f); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid form data")
	}
	f.Next = middleware.SafeNext(f.Next)

	if errs := h.FormRules.Validate(c); errs != nil {
		return respond.HTML(c, http.StatusUnprocessableEntity, loginForm(f, errs))
	}

	log := logging.FromContext(c.Request().Context())

	session, err := h.orch.Login(c.Request().Context(), f.Email, f.Password)
	if _, bad := errs.IsInvalid(err); bad {
		log.Warn("login failed", "email", f.Email)
		// 422 like any form error: htmx swaps it in (components.htmxConfig);
		// a 401 was dropped and the page showed nothing.
		return respond.HTML(c, http.StatusUnprocessableEntity, loginForm(f, map[string]string{
			"general": "Invalid email or password",
		}))
	}
	if err != nil {
		log.Error("login failed", "error", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "session error")
	}

	auth.SetSession(c, h.orch.Sessions(), session)
	return respond.Redirect(c, cmp.Or(f.Next, "/"))
}

// POST /logout
func (h *handler) Logout(c echo.Context) error {
	sm := h.orch.Sessions()
	if cookie, err := c.Cookie(sm.CookieName()); err == nil {
		_ = h.orch.Logout(c.Request().Context(), cookie.Value)
	}

	auth.ClearSession(c, sm)
	hamrmw.SetFlash(c, "You have been logged out", hamrmw.FlashInfo)
	return respond.Redirect(c, "/login")
}

// Login tries per window: per address and email (guessing one account), and
// per address across emails (one password sprayed over many). Behind a proxy
// the panel was not told about (the installer's CDN question), every visitor
// shares one address; keying on the email too keeps that from locking the
// whole site out, at worst one account.
// ponytail: in memory, so a restart clears it; one panel process per box.
const (
	triesPerAccount = 10
	triesPerAddress = 100
	triesWindow     = 15 * time.Minute
)

// Limit guards POST /login with hamr's fixed-window store, keyed on the
// client's real address (TRUSTED_PROXIES decides whose X-Forwarded-For counts).
// Past a limit the form says when to come back, and no password is checked.
func (h *handler) Limit() echo.MiddlewareFunc {
	store := hamrmw.NewMemoryStore(hamrmw.WithMaxSize(20000))
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			ctx, ip := c.Request().Context(), c.RealIP()
			email := strings.ToLower(strings.TrimSpace(c.FormValue("email")))
			okAcct, _, resetAcct, err1 := store.Allow(ctx, "acct:"+ip+"|"+email, triesPerAccount, triesWindow)
			okAddr, _, resetAddr, err2 := store.Allow(ctx, "addr:"+ip, triesPerAddress, triesWindow)
			if err1 != nil || err2 != nil || (okAcct && okAddr) {
				return next(c)
			}
			reset := resetAcct
			if !okAddr && resetAddr.After(reset) {
				reset = resetAddr
			}
			wait := max(1, int(time.Until(reset).Round(time.Minute).Minutes()))
			c.Response().Header().Set("Retry-After", strconv.Itoa(int(time.Until(reset).Seconds())+1))
			f := LoginForm{Email: c.FormValue("email"), Next: middleware.SafeNext(c.FormValue("next"))}
			return respond.HTML(c, http.StatusTooManyRequests, loginForm(f, map[string]string{
				"general": fmt.Sprintf("Too many tries. Try again in %d minute%s.", wait, plural(wait)),
			}))
		}
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
