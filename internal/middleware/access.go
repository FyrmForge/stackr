package middleware

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/FyrmForge/hamr/pkg/ctx"
	"github.com/FyrmForge/hamr/pkg/logging"
	hamrmw "github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
)

// Access is the one auth middleware both routers mount. Load finds the
// principal (API key first, then the browser session); Require resolves
// /:org/:stack/:env/:tile, asks authz.Can, and leaves the rows in the
// context so handlers never re-fetch.
type Access struct {
	svc     *service.Orchestrator
	browser *hamrmw.BrowserAuth
}

var scopeKey = ctx.NewKey[service.Scope]("scope")

func NewAccess(svc *service.Orchestrator) *Access {
	return &Access{svc: svc, browser: hamrmw.NewBrowserAuth(svc.Sessions(),
		hamrmw.WithSubjectLoader(func(c context.Context, id string) (any, error) {
			p, err := svc.SessionPrincipal(c, id)
			if errors.Is(err, errs.ErrNotFound) {
				return nil, nil // user gone: hamr treats the session as stale
			}
			if err != nil {
				return nil, err
			}
			return p, nil
		}),
		hamrmw.WithLoginRedirect("/login"),
		hamrmw.WithHomeRedirect("/"),
	)}
}

// Browser is hamr's session auth, for RequireAuth/RequireNotAuth on pages.
func (a *Access) Browser() *hamrmw.BrowserAuth { return a.browser }

// Load puts the principal in the context. A bearer token that matches no
// key is a 401; no credentials at all is an anonymous request.
func (a *Access) Load() echo.MiddlewareFunc {
	session := a.browser.Load()
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		viaSession := session(next)
		return func(c echo.Context) error {
			token, ok := strings.CutPrefix(c.Request().Header.Get(echo.HeaderAuthorization), "Bearer ")
			if !ok {
				return viaSession(c)
			}
			p, err := a.svc.KeyPrincipal(c.Request().Context(), strings.TrimSpace(token))
			if errors.Is(err, errs.ErrNotFound) {
				return echo.NewHTTPError(http.StatusUnauthorized, "invalid API key")
			}
			if err != nil {
				return err
			}
			ctx.Set(c, ctx.SubjectKey, any(p))
			ctx.Set(c, ctx.SubjectIDKey, p.User.ID)
			return next(c)
		}
	}
}

// Require gates a route on one verb. Mount after Load.
func (a *Access) Require(v authz.Verb) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			p := Principal(c)
			if p == nil {
				return echo.NewHTTPError(http.StatusUnauthorized)
			}
			s, err := a.svc.Resolve(c.Request().Context(), c.Param("org"), c.Param("stack"), c.Param("env"), c.Param("tile"))
			if err != nil {
				return HTTPError(err)
			}
			var r authz.Resource
			if s.Org != nil {
				r.OrgID = s.Org.ID
			}
			if err := authz.Can(p.Access, v, r); err != nil {
				return HTTPError(err)
			}
			if err := a.children(c, s); err != nil {
				return err
			}
			ctx.Set(c, scopeKey, s)
			return next(c)
		}
	}
}

// Authed gates the routes about the caller alone: /me, the org list.
func (a *Access) Authed() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			p := Principal(c)
			if p == nil {
				return echo.NewHTTPError(http.StatusUnauthorized)
			}
			if err := authz.Self(p.Access); err != nil {
				return HTTPError(err)
			}
			return next(c)
		}
	}
}

// LoginFirst sends an anonymous page load to the login page, which comes
// back here after (DECIDE 96). htmx, stream and API requests keep their 401:
// only a full GET is a page. Mount before Require/Authed.
func (a *Access) LoginFirst() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			r := c.Request()
			if Principal(c) != nil || r.Method != http.MethodGet || r.Header.Get("HX-Request") != "" {
				return next(c)
			}
			to := "/login"
			if u := r.URL.RequestURI(); u != "/" {
				to += "?next=" + url.QueryEscape(u)
			}
			return c.Redirect(http.StatusSeeOther, to)
		}
	}
}

// SafeNext is a login's way back: a path on this site, never another host
// ("//x", a backslash or a control character a browser would fold into
// one) or a scheme; "" when it is not one.
func SafeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") ||
		strings.ContainsFunc(next, func(r rune) bool { return r == '\\' || r < 0x20 || r == 0x7f }) {
		return ""
	}
	return next
}

// childKinds are the path params naming a row the verb takes on trust; each
// must sit in the route's org. The value is the kind OrgOf knows.
var childKinds = map[string]string{
	"job": "job", "release": "release", "domain": "domain", "volume": "volume",
	"schedule": "schedule", "provision": "provision",
}

// byVerb are path params the verb itself scopes (it takes the org or the
// user alongside the id), or that name no row.
var byVerb = map[string]bool{
	"org": true, "stack": true, "env": true, "tile": true,
	"user": true, "credential": true, "connector": true, "dest": true, "key": true,
	"collection": true, "name": true, "token": true, "setting": true,
	"run": true, // the verb takes the tile too and refuses another tile's run
}

// KnownParam reports whether a route param is org-checked or verb-scoped;
// the route test holds every /api/v1 path to it.
func KnownParam(name string) bool { _, child := childKinds[name]; return child || byVerb[name] }

// children refuses a child id from another org with the same 404 as a
// missing one. An unlisted param fails closed.
// ponytail: org-level only; v1 roles are per org, so an id of another tile
// in the same org reaches nothing the caller cannot already reach.
func (a *Access) children(c echo.Context, s service.Scope) error {
	for _, name := range c.ParamNames() {
		kind, child := childKinds[name]
		if !child {
			if byVerb[name] {
				continue
			}
			return echo.NewHTTPError(http.StatusInternalServerError, "route param "+name+" has no org check")
		}
		if s.Org == nil {
			continue // an org-less route: its verb is admin-level
		}
		org, err := a.svc.OrgOf(c.Request().Context(), kind, c.Param(name))
		if err != nil && !errors.Is(err, errs.ErrNotFound) {
			return err
		}
		if err != nil || org != s.Org.ID {
			return echo.NewHTTPError(http.StatusNotFound)
		}
	}
	return nil
}

// Principal is the loaded principal, nil for an anonymous request.
func Principal(c echo.Context) *service.Principal {
	p, _ := hamrmw.GetSubject(c).(*service.Principal)
	return p
}

// ScopeOf is what Require resolved.
func ScopeOf(c echo.Context) service.Scope {
	s, _ := ctx.Get(c, scopeKey)
	return s
}

// HTTPError maps the service vocabulary to a status, once, at the edge.
func HTTPError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errs.ErrNotFound):
		return echo.NewHTTPError(http.StatusNotFound)
	case errors.Is(err, errs.ErrRefused):
		return echo.NewHTTPError(http.StatusForbidden, err.Error())
	case errors.Is(err, errs.ErrBusy):
		return echo.NewHTTPError(http.StatusServiceUnavailable)
	case errors.Is(err, service.ErrBadSignature):
		return echo.NewHTTPError(http.StatusUnauthorized, "bad signature")
	case errors.Is(err, service.ErrBadPayload):
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if v, ok := errs.IsInvalid(err); ok {
		return echo.NewHTTPError(http.StatusBadRequest, v.Error())
	}
	var be *echo.BindingError
	if errors.As(err, &be) {
		return echo.NewHTTPError(http.StatusBadRequest, be.Field+": "+fmt.Sprint(be.Message))
	}
	if v, ok := errs.IsConflict(err); ok {
		return echo.NewHTTPError(http.StatusConflict, v.Msg)
	}
	if v, ok := errs.IsUnset(err); ok {
		return echo.NewHTTPError(http.StatusConflict, v.Error())
	}
	return err
}

// APIError is every API error body.
type APIError struct {
	Error  string `json:"error"`
	Status int    `json:"status"`
	Field  string `json:"field,omitempty"`
}

// JSONErrors renders every error under /api as an APIError, through
// HTTPError, so the CLI prints the service's typed message.
func JSONErrors() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			err := next(c)
			if err == nil || c.Response().Committed {
				return err
			}
			body := APIError{Status: http.StatusInternalServerError, Error: "internal error"}
			if v, ok := errs.IsInvalid(err); ok {
				body.Field = v.Field
			}
			var he *echo.HTTPError
			if errors.As(HTTPError(err), &he) {
				body.Status = he.Code
				body.Error = http.StatusText(he.Code)
				if m, ok := he.Message.(string); ok && m != "" {
					body.Error = m
				}
			} else {
				logging.FromContext(c.Request().Context()).Error("api", "error", err.Error())
			}
			return c.JSON(body.Status, body)
		}
	}
}

// APICSRF is the web's CSRF check for API calls a browser session makes.
// Mount after Load: a bearer key is not ambient and an anonymous call
// carries nothing to ride on, so both skip it.
func APICSRF(secure bool) echo.MiddlewareFunc {
	csrf := hamrmw.CSRFWithConfig(hamrmw.CSRFConfig{Secure: secure})
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		checked := csrf(next)
		return func(c echo.Context) error {
			if Principal(c) == nil || strings.HasPrefix(c.Request().Header.Get(echo.HeaderAuthorization), "Bearer ") {
				return next(c)
			}
			return checked(c)
		}
	}
}
