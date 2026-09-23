package middleware

import (
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/service"
)

// WebActor names who is acting from the panel: the id and display name a
// staged change is signed with, the address a run row is signed with, and the
// surface — which is what makes a panel edit queue for review on the canvas
// while the same edit over the API lands immediately.
//
// One function rather than one per page package: every panel handler that
// writes through a service needs exactly this, and three copies of it is how
// the surfaces drifted in the first place.
func WebActor(c echo.Context) service.Actor {
	a := service.Actor{Via: "web"}
	if u := CurrentUser(c); u != nil {
		a.ID, a.Name, a.Email = u.ID, u.Name, u.Email
	}
	return a
}
