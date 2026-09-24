// Package auth holds web-layer auth helpers shared across the
// internal/web/handler/auth/* page packages (login, register, etc.).
//
// Pure service-layer code (Authenticate, Register flows) lives in
// internal/service.
package auth

import (
	"net/http"

	hamrauth "github.com/FyrmForge/hamr/pkg/auth"
	"github.com/labstack/echo/v4"
)

// SetSession writes the session cookie using the session manager's config.
// Called after Login and Register hand off to the user's session.
func SetSession(c echo.Context, sm *hamrauth.SessionManager, session *hamrauth.Session) {
	c.SetCookie(&http.Cookie{
		Name:     sm.CookieName(),
		Value:    session.Token,
		Path:     sm.CookiePath(),
		Domain:   sm.CookieDomain(),
		HttpOnly: true,
		Secure:   sm.CookieSecure(),
		SameSite: sm.SameSite(),
	})
}

// ClearSession deletes the session cookie. Called by Logout.
func ClearSession(c echo.Context, sm *hamrauth.SessionManager) {
	c.SetCookie(&http.Cookie{
		Name:     sm.CookieName(),
		Value:    "",
		Path:     sm.CookiePath(),
		Domain:   sm.CookieDomain(),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   sm.CookieSecure(),
		SameSite: sm.SameSite(),
	})
}
