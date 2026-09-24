package middleware

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/FyrmForge/hamr/pkg/ctx"
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
			ctx.Set(c, scopeKey, s)
			return next(c)
		}
	}
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
	}
	if v, ok := errs.IsInvalid(err); ok {
		return echo.NewHTTPError(http.StatusBadRequest, v.Error())
	}
	if v, ok := errs.IsConflict(err); ok {
		return echo.NewHTTPError(http.StatusConflict, v.Msg)
	}
	if v, ok := errs.IsUnset(err); ok {
		return echo.NewHTTPError(http.StatusConflict, v.Error())
	}
	return err
}
