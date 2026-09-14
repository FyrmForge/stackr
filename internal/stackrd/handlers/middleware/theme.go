package middleware

import (
	"context"

	"github.com/labstack/echo/v4"
)

type themeKey struct{}

// ThemeContext copies the signed-in user's theme onto the request context.
//
// Components normally read the theme off the echo context, but the error page
// cannot: hamr renders it through hamrmw.ErrorPages, whose ErrorPage signature
// is (code, message) with no echo.Context in it. So every error page fell back
// to prefers-color-scheme and served the OS palette to an account that had
// pinned the other one. respond.HTML renders with c.Request().Context(), which
// is the one thing the component does get.
//
// Must be registered after the auth loader, or there is no user to read.
func ThemeContext() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			u := CurrentUser(c)
			if u == nil || u.Theme == "" {
				return next(c)
			}
			r := c.Request()
			c.SetRequest(r.WithContext(context.WithValue(r.Context(), themeKey{}, u.Theme)))
			return next(c)
		}
	}
}

// Theme returns the theme stored by ThemeContext ("" when unset).
func Theme(ctx context.Context) string {
	t, _ := ctx.Value(themeKey{}).(string)
	return t
}
