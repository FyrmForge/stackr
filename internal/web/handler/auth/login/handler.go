package login

import (
	"cmp"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
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
			validate.Field("email",
				validate.WithMsg(validate.Required, "Enter your email."),
				validate.WithMsg(validate.Email, "Enter an email like name@example.com.")),
			validate.FieldMsg("password", "Enter your password.", validate.Required),
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
		c.Set(failedKey, true)
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

// Failed logins per window: per address and email (guessing one account), and
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

// failedKey marks a request whose password was wrong; only those count.
const failedKey = "login.failed"

// tries is a fixed window of login tries per key. Every try takes a slot up
// front, so a burst of parallel guesses cannot all pass the check, and a
// right password gives its slot back.
// ponytail: in memory, one global lock; pruned only when full, and a full
// map refuses new keys until windows end. A shared store if logins scale.
type tries struct {
	mu sync.Mutex
	m  map[string]window
}

type window struct {
	n     int
	reset time.Time
}

const maxKeys = 20000

// take counts a try on key unless it has reached limit; it says whether
// the try may go on and when the window ends. A full map past pruning
// refuses new keys, so a flood cannot grow it; keys it holds still count.
func (t *tries) take(key string, limit int) (bool, time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	w, ok := t.m[key]
	if !ok && len(t.m) >= maxKeys {
		for k, x := range t.m {
			if !now.Before(x.reset) {
				delete(t.m, k)
			}
		}
		if len(t.m) >= maxKeys {
			return false, now.Add(triesWindow)
		}
	}
	if !now.Before(w.reset) {
		w = window{reset: now.Add(triesWindow)}
	}
	if w.n >= limit {
		return false, w.reset
	}
	w.n++
	t.m[key] = w
	return true, w.reset
}

// give hands back a try taken on key; a key with none left goes.
func (t *tries) give(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch w, ok := t.m[key]; {
	case !ok:
	case w.n <= 1:
		delete(t.m, key)
	default:
		w.n--
		t.m[key] = w
	}
}

// Limit guards POST /login, keyed on the client's real address
// (TRUSTED_PROXIES decides whose X-Forwarded-For counts). Only a wrong
// password counts, so people signing in fine never lock each other out.
// Past a limit the form says when to come back, and no password is checked.
func (h *handler) Limit() echo.MiddlewareFunc {
	t := &tries{m: map[string]window{}}
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			ip := c.RealIP()
			email := strings.ToLower(strings.TrimSpace(c.FormValue("email")))
			acct, addr := "acct:"+ip+"|"+email, "addr:"+ip
			// The address first: a blocked address adds no key per email.
			okAddr, reset := t.take(addr, triesPerAddress)
			if okAddr {
				okAcct, resetAcct := t.take(acct, triesPerAccount)
				if okAcct {
					err := next(c)
					if failed, _ := c.Get(failedKey).(bool); !failed {
						t.give(acct)
						t.give(addr)
					}
					return err
				}
				t.give(addr)
				reset = resetAcct
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
